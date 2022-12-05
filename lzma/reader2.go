package lzma

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
)

// Reader2Config provides the dictionary size parameter for a LZMA2 reader.
//
// Note that the parallel decoding will only work if the stream has been encoded
// with multiple workers and the WorkerBufferSize is large enough. If the worker
// buffer size is too small only one worker will be used for decompression.
type Reader2Config struct {
	// DictSize provides the maximum dictionary size supported.
	DictSize int
	// Workers gives the maximum number of decompressing workers.
	Workers int
	// WorkerBufferSize give the maximum size of uncompressed data that can be
	// decoded by a single worker.
	WorkerBufferSize int
}

// Verify checks the validity of dictionary size.
func (cfg *Reader2Config) Verify() error {
	if cfg.DictSize < minDictSize {
		return fmt.Errorf(
			"lzma: dictionary size must be larger or"+
				" equal %d bytes", minDictSize)
	}

	if cfg.Workers < 0 {
		return errors.New("lzma: Worker must be larger than 0")
	}

	if cfg.WorkerBufferSize <= 0 {
		return errors.New(
			"lzma: WorkerBufferSize must be greater than 0")
	}

	return nil
}

// ApplyDefaults sets a default value for the dictionary size. Note that
// multi-threaded readers are not the default.
func (cfg *Reader2Config) ApplyDefaults() {
	if cfg.DictSize == 0 {
		cfg.DictSize = 8 << 20
	}

	if cfg.Workers == 0 {
		cfg.Workers = 1
	}

	if cfg.WorkerBufferSize == 0 {
		cfg.WorkerBufferSize = 1 << 20
	}
}

// NewReader2 creates a LZMA2 reader. Note that the interface is a ReadCloser,
// so it has to be closed after usage.
func NewReader2(z io.Reader, dictSize int) (r io.ReadCloser, err error) {
	return NewReader2Config(z, Reader2Config{DictSize: dictSize})
}

// NewReader2Config generates an LZMA2 reader using the configuration parameter
// attribute. Note that the code returns a ReadCloser, which has to be clsoed
// after reading.
func NewReader2Config(z io.Reader, cfg Reader2Config) (r io.ReadCloser, err error) {
	cfg.ApplyDefaults()
	if err = cfg.Verify(); err != nil {
		return nil, err
	}
	if cfg.Workers <= 1 {
		var cr chunkReader
		cr.init(z, cfg.DictSize)
		return io.NopCloser(&cr), nil
	}
	return newMTReader(cfg, z), nil
}

// mtReaderTask describes a single decompression task.
type mtReaderTask struct {
	// compressed stream consisting of chunks
	z io.Reader
	// uncompressed size; less than zero if unknown.
	size int
	// reader for decompressed data
	rCh chan io.Reader
}

// mtReader provides a multithreaded reader for LZMA2 streams.
type mtReader struct {
	cancel context.CancelFunc
	outCh  <-chan mtReaderTask
	err    error
	r      io.Reader
}

// newMTReader creates a new multithreader reader. Note that Close must be
// called to clean up.
func newMTReader(cfg Reader2Config, z io.Reader) *mtReader {
	ctx, cancel := context.WithCancel(context.Background())
	tskCh := make(chan mtReaderTask)
	outCh := make(chan mtReaderTask)
	go mtrGenerate(ctx, z, cfg, tskCh, outCh)
	return &mtReader{
		cancel: cancel,
		outCh:  outCh,
	}
}

// Read reads the data from the multithreaded reader.
func (r *mtReader) Read(p []byte) (n int, err error) {
	if r.err != nil {
		return 0, r.err
	}
	for n < len(p) {
		if r.r == nil {
			tsk, ok := <-r.outCh
			if !ok {
				r.err = io.EOF
				if n == 0 {
					r.cancel()
					return 0, io.EOF
				}
				return n, nil
			}
			r.r = <-tsk.rCh
		}
		k, err := r.r.Read(p[n:])
		n += k
		if err != nil {
			if err == io.EOF {
				r.r = nil
				continue
			}
			r.err = err
			return n, err
		}
	}
	return n, nil
}

// Close closes the multihreaded reader and stops all workers.
func (r *mtReader) Close() error {
	if r.err == errClosed {
		return errClosed
	}
	r.cancel()
	r.err = errClosed
	return nil
}

// mtrGenerate generates the tasks for the multithreaded reader. It should be
// started as go routine.
func mtrGenerate(ctx context.Context, z io.Reader, cfg Reader2Config, tskCh, outCh chan mtReaderTask) {
	r := bufio.NewReader(z)
	workers := 0
	for ctx.Err() == nil {
		buf := new(bytes.Buffer)
		buf.Grow(cfg.WorkerBufferSize)
		tsk := mtReaderTask{
			rCh: make(chan io.Reader, 1),
		}
		size, parallel, err := splitStream(buf, r, cfg.WorkerBufferSize)
		if err != nil && err != io.EOF {
			tsk.rCh <- &errReader{err: err}
			select {
			case <-ctx.Done():
				return
			case outCh <- tsk:
			}
			close(outCh)
			return
		}
		if parallel {
			if workers < cfg.Workers {
				go mtrWork(ctx, cfg.DictSize, tskCh)
				workers++
			}
			tsk.z = buf
			tsk.size = size
			select {
			case <-ctx.Done():
				return
			case tskCh <- tsk:
			}
			select {
			case <-ctx.Done():
				return
			case outCh <- tsk:
			}
			if err == io.EOF {
				close(outCh)
				return
			}
		} else {
			tsk.z = io.MultiReader(buf, r)
			tsk.size = -1
			chr := new(chunkReader)
			chr.init(tsk.z, cfg.DictSize)
			chr.noEOS = false
			select {
			case <-ctx.Done():
				return
			case tsk.rCh <- chr:
			}
			select {
			case <-ctx.Done():
				return
			case outCh <- tsk:
			}
			close(outCh)
			return
		}
	}
}

// errReader is a reader that returns only an error.
type errReader struct{ err error }

// Read returns the error of the errReader.
func (r *errReader) Read(p []byte) (n int, err error) { return 0, r.err }

// mtrWork is the go routine function that does the actual decompression.
func mtrWork(ctx context.Context, dictSize int, tskCh <-chan mtReaderTask) {
	var chr chunkReader
	chr.init(nil, dictSize)
	for {
		var tsk mtReaderTask
		select {
		case <-ctx.Done():
			return
		case tsk = <-tskCh:
		}
		chr.reset(tsk.z)
		if tsk.size >= 0 {
			chr.noEOS = true
			buf := new(bytes.Buffer)
			buf.Grow(int(tsk.size))
			var r io.Reader
			if _, err := io.Copy(buf, &chr); err != nil {
				r = &errReader{err: err}
			} else {
				r = buf
			}
			select {
			case <-ctx.Done():
				return
			case tsk.rCh <- r:
			}
		} else {
			panic(fmt.Errorf("negative size not expexted"))
		}
	}
}

// splitStream splits the LZMA stream into blocks that can be processed in
// parallel. Such blocks need to start with a dictionary reset. If such a block
// cannot be found that is less or equal size then false is returned and the
// write contains a series of chunks and the last chunk headere. The number n
// contains the size of the decompressed block. If ok is false n will be zero.
func splitStream(w io.Writer, z *bufio.Reader, size int) (n int, ok bool, err error) {
	for {
		hdr, k, err := peekChunkHeader(z)
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return 0, false, err
		}
		switch hdr.control {
		case cUD, cCSPD:
			if n > 0 {
				return n, true, nil
			}
		case cEOS:
			return n, true, io.EOF
		}
		if hdr.control&(1<<7) == 0 {
			k += hdr.size
		} else {
			k += hdr.compressedSize
		}
		n += hdr.size
		if n > size {
			return 0, false, io.EOF
		}
		if _, err := io.CopyN(w, z, int64(k)); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return 0, false, err
		}
	}
}