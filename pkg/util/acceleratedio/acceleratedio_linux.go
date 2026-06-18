package acceleratedio

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const (
	blkGetSize64     = 0x80081272
	progressTick     = time.Second
	defaultBlockSize = 64 * 1024
	defaultWorkers   = 4
)

type readTask struct {
	index  int
	offset int64
	size   int
}

type readResult struct {
	index int
	data  []byte
	err   error
}

type writeTask struct {
	index  int
	offset int64
	data   []byte
}

func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("io-mode", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var devicePath string
	var blockSize int
	var workers int
	var mode string

	fs.StringVar(&devicePath, "device", "", "Path to block device")
	fs.IntVar(&blockSize, "bs", defaultBlockSize, "Block size in bytes")
	fs.IntVar(&workers, "workers", defaultWorkers, "Number of concurrent workers")
	fs.StringVar(&mode, "mode", "", "Mode: read or write")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if devicePath == "" {
		return errors.New("no device specified, use -device")
	}
	if blockSize <= 0 {
		return errors.New("block size must be greater than zero")
	}
	if workers <= 0 {
		return errors.New("workers must be greater than zero")
	}

	switch mode {
	case "read":
		return readBlockDevice(devicePath, blockSize, workers, stdout, stderr)
	case "write":
		return writeBlockDevice(devicePath, blockSize, workers, stdin, stderr)
	default:
		return errors.New("invalid mode, use -mode=read or -mode=write")
	}
}

func blockDeviceSize(f *os.File) (int64, error) {
	var size int64
	_, _, errNo := syscall.Syscall(
		syscall.SYS_IOCTL,
		f.Fd(),
		blkGetSize64,
		uintptr(unsafe.Pointer(&size)),
	)
	if errNo != 0 {
		return 0, errNo
	}
	return size, nil
}

func readWorker(file *os.File, tasks <-chan readTask, results chan<- readResult) {
	for task := range tasks {
		buf := make([]byte, task.size)
		n, err := file.ReadAt(buf, task.offset)
		if err != nil && !errors.Is(err, io.EOF) {
			results <- readResult{index: task.index, err: err}
			continue
		}
		results <- readResult{index: task.index, data: buf[:n]}
	}
}

func writeWorker(device *os.File, tasks <-chan writeTask, errCh chan<- error, wg *sync.WaitGroup) {
	defer wg.Done()
	for task := range tasks {
		n, err := device.WriteAt(task.data, task.offset)
		if err != nil {
			errCh <- fmt.Errorf("error writing block %d at offset %d: %w", task.index, task.offset, err)
			return
		}
		if n != len(task.data) {
			errCh <- fmt.Errorf("short write for block %d at offset %d", task.index, task.offset)
			return
		}
	}
}

func runProgressTicker(done <-chan struct{}, current func() int64, totalSize int64, label string, stderr io.Writer) {
	ticker := time.NewTicker(progressTick)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			written := current()
			var percent float64
			if totalSize > 0 {
				percent = float64(written) / float64(totalSize) * 100
			}
			// Structured prefix `IO_PROGRESS` lets the controller parse the latest line
			// from pod logs to surface progress on the VolumeBackup/VolumeRestore status.
			fmt.Fprintf(stderr, "IO_PROGRESS mode=%s bytes=%d total=%d percent=%.2f\n", label, written, totalSize, percent)
		case <-done:
			fmt.Fprintln(stderr)
			return
		}
	}
}

func startProgress(current func() int64, totalSize int64, label string, stderr io.Writer) func() {
	done := make(chan struct{})
	go runProgressTicker(done, current, totalSize, label, stderr)
	return func() { close(done) }
}

func produceReadTasks(tasks chan<- readTask, totalSize int64, blockSize int) {
	defer close(tasks)
	for offset, index := int64(0), 0; offset < totalSize; index++ {
		size := blockSize
		if offset+int64(size) > totalSize {
			size = int(totalSize - offset)
		}
		tasks <- readTask{index: index, offset: offset, size: size}
		offset += int64(size)
	}
}

func dispatchWriteTasks(stdin io.Reader, blockSize int, tasks chan<- writeTask, errCh <-chan error, bytesWritten *int64) error {
	index := 0
	offset := int64(0)
	for {
		select {
		case err := <-errCh:
			return err
		default:
		}

		buf := make([]byte, blockSize)
		n, err := io.ReadFull(stdin, buf)
		if n > 0 {
			tasks <- writeTask{index: index, offset: offset, data: buf[:n]}
			atomic.AddInt64(bytesWritten, int64(n))
			offset += int64(n)
			index++
		}
		switch {
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
			return firstError(errCh)
		case err != nil:
			return fmt.Errorf("error reading input: %w", err)
		}
	}
}

func getSize(file *os.File) (int64, error) {
	totalSize, err := blockDeviceSize(file)
	if err == nil {
		return totalSize, nil
	}
	stat, statErr := file.Stat()
	if statErr != nil {
		return 0, fmt.Errorf("error stat'ing device: %w", statErr)
	}
	return stat.Size(), nil
}

func readBlockDevice(devicePath string, blockSize, workers int, stdout, stderr io.Writer) error {
	file, err := os.Open(devicePath)
	if err != nil {
		return fmt.Errorf("error opening device: %w", err)
	}
	defer file.Close()

	totalSize, err := getSize(file)
	if err != nil {
		return err
	}

	tasks := make(chan readTask, workers)
	results := make(chan readResult, workers)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			readWorker(file, tasks, results)
		}()
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	go produceReadTasks(tasks, totalSize, blockSize)

	var bytesRead int64
	defer startProgress(func() int64 {
		return atomic.LoadInt64(&bytesRead)
	}, totalSize, "READ", stderr)()

	expected := 0
	buffer := map[int][]byte{}
	flush := func(data []byte) error {
		if _, err := stdout.Write(data); err != nil {
			return err
		}
		atomic.AddInt64(&bytesRead, int64(len(data)))
		expected++
		return nil
	}
	for res := range results {
		if res.err != nil {
			return fmt.Errorf("error in read worker for block %d: %w", res.index, res.err)
		}
		if res.index != expected {
			buffer[res.index] = res.data
			continue
		}
		if err := flush(res.data); err != nil {
			return err
		}
		for {
			data, ok := buffer[expected]
			if !ok {
				break
			}
			delete(buffer, expected)
			if err := flush(data); err != nil {
				return err
			}
		}
	}

	return nil
}

func writeBlockDevice(devicePath string, blockSize, workers int, stdin io.Reader, stderr io.Writer) error {
	device, err := os.OpenFile(devicePath, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("error opening device for writing: %w", err)
	}
	defer device.Close()

	totalSize, err := getSize(device)
	if err != nil {
		return err
	}

	tasks := make(chan writeTask, workers)
	errCh := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go writeWorker(device, tasks, errCh, &wg)
	}

	var bytesWritten int64
	defer startProgress(func() int64 {
		return atomic.LoadInt64(&bytesWritten)
	}, totalSize, "WRITE", stderr)()

	err = dispatchWriteTasks(stdin, blockSize, tasks, errCh, &bytesWritten)
	// Drain workers before fsync so every WriteAt has issued by the time we
	// flush. close(tasks) signals workers to exit, wg.Wait() blocks until they
	// have. Done explicitly (not via defer) so Sync can run before Close.
	close(tasks)
	wg.Wait()
	if err != nil {
		return err
	}
	// fsync the device so dirty pages reach the underlying storage before we
	// declare success. Without this the Pod can exit with cached writes still
	// in flight, and a downstream consumer (e.g. a VM attaching the restored
	// PVC immediately) may read stale or partial data and fail to boot.
	if err := device.Sync(); err != nil {
		return fmt.Errorf("error syncing device: %w", err)
	}
	return nil
}

func firstError(errCh <-chan error) error {
	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}
