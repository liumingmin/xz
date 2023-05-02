// Copyright 2014-2021 Ulrich Kunitz. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package xz

import (
	"errors"
	"fmt"
	"io"

	"github.com/ulikunitz/xz/lzma"
)

// LZMA filter constants.
const (
	lzmaFilterID  = 0x21
	lzmaFilterLen = 3
)

// lzmaFilter declares the LZMA2 filter information stored in an xz
// block header.
type lzmaFilter struct {
	dictSize int64
}

// String returns a representation of the LZMA filter.
func (f lzmaFilter) String() string {
	return fmt.Sprintf("LZMA dict cap %#x", f.dictSize)
}

// id returns the ID for the LZMA2 filter.
func (f lzmaFilter) id() uint64 { return lzmaFilterID }

// MarshalBinary converts the lzmaFilter in its encoded representation.
func (f lzmaFilter) MarshalBinary() (data []byte, err error) {
	c := lzma.EncodeDictSize(f.dictSize)
	return []byte{lzmaFilterID, 1, c}, nil
}

// UnmarshalBinary unmarshals the given data representation of the LZMA2
// filter.
func (f *lzmaFilter) UnmarshalBinary(data []byte) error {
	if len(data) != lzmaFilterLen {
		return errors.New("xz: data for LZMA2 filter has wrong length")
	}
	if data[0] != lzmaFilterID {
		return errors.New("xz: wrong LZMA2 filter id")
	}
	if data[1] != 1 {
		return errors.New("xz: wrong LZMA2 filter size")
	}
	dc, err := lzma.DecodeDictSize(data[2])
	if err != nil {
		return errors.New("xz: wrong LZMA2 dictionary size property")
	}

	f.dictSize = dc
	return nil
}

// reader creates a new reader for the LZMA2 filter.
func (f lzmaFilter) reader(r io.Reader, c *ReaderConfig) (fr io.ReadCloser, err error) {

	var cfg lzma.Reader2Config
	if c != nil {
		cfg = lzma.Reader2Config{
			Workers:    c.Workers,
			WorkSize:   c.LZMAWorkSize,
		}
	}
	dc := int(f.dictSize)
	if dc < 1 {
		return nil, errors.New(
			"xz: LZMA2 filter parameter dictionary capacity overflow")
	}
	cfg.WindowSize = dc

	fr, err = lzma.NewReader2Config(r, cfg)
	if err != nil {
		return nil, err
	}
	return fr, nil
}

// writeCloser creates a io.WriteCloser for the LZMA2 filter.
func (f lzmaFilter) writeCloser(w io.WriteCloser, c *WriterConfig,
) (fw io.WriteCloser, err error) {
	var cfg lzma.Writer2Config
	if c != nil {
		cfg = c.LZMA
	} else {
		cfg.SetDefaults()
	}

	dc := int(f.dictSize)
	if dc < 1 {
		return nil, errors.New("xz: LZMA2 filter parameter " +
			"dictionary capacity overflow")
	}

	bc := cfg.ParserConfig.BufConfig()
	if dc > bc.WindowSize {
		bc.WindowSize = dc
		cfg.ParserConfig.SetBufConfig(bc)
		// TODO: adjust buffer size?
	}

	fw, err = lzma.NewWriter2Config(w, cfg)
	if err != nil {
		return nil, err
	}
	return fw, nil
}

// last returns true, because an LZMA2 filter must be the last filter in
// the filter list.
func (f lzmaFilter) last() bool { return true }
