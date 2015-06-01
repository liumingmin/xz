package lzma

import (
	"errors"
)

// Parameters contain all information required to decode or encode an LZMA
// stream.
//
// The sum of DictSize and ExtraBufSize must be less or equal MaxInt32 on
// 32-bit platforms.
type Parameters struct {
	// number of literal context bits
	LC int
	// number of literal position bits
	LP int
	// number of position bits
	PB int
	// size of the dictionary in bytes
	DictSize int64
	// size of uncompressed data in bytes
	Size int64
	// header includes unpacked size
	SizeInHeader bool
	// end-of-stream marker requested
	EOS bool
	// additional buffer size on top of dictionary size
	ExtraBufSize int64
}

// Properties returns LC, LP and PB as Properties value.
func (p *Parameters) Properties() Properties {
	props, err := NewProperties(p.LC, p.LP, p.PB)
	if err != nil {
		panic(err)
	}
	return props
}

// SetProperties sets the LC, LP and PB fields.
func (p *Parameters) SetProperties(props Properties) {
	p.LC, p.LP, p.PB = props.LC(), props.LP(), props.PB()
}

func (p *Parameters) normalizeDictSize() {
	if p.DictSize == 0 {
		p.DictSize = Default.DictSize
	}
	if p.DictSize < MinDictSize {
		p.DictSize = MinDictSize
	}
}

func (p *Parameters) normalizeReaderExtraBufSize() {
	if p.ExtraBufSize < 0 {
		p.ExtraBufSize = 0
	}
}

func (p *Parameters) normalizeWriterExtraBufSize() {
	if p.ExtraBufSize <= 0 {
		p.ExtraBufSize = 4096
	}
}

func (p *Parameters) normalizeReaderSizes() {
	p.normalizeDictSize()
	p.normalizeReaderExtraBufSize()
}

func (p *Parameters) normalizeWriterSizes() {
	p.normalizeDictSize()
	p.normalizeWriterExtraBufSize()
}

// Verify checks parameters for errors.
func (p *Parameters) Verify() error {
	if p == nil {
		return errors.New("parameters must be non-nil")
	}
	if err := verifyProperties(p.LC, p.LP, p.PB); err != nil {
		return err
	}
	if !(MinDictSize <= p.DictSize && p.DictSize <= MaxDictSize) {
		return errors.New("DictSize out of range")
	}
	if p.DictSize != int64(int(p.DictSize)) {
		return errors.New("DictSize too large for int")
	}
	if p.Size < 0 {
		return errors.New("Size must not be negative")
	}
	if p.ExtraBufSize < 0 {
		return errors.New("ExtraBufSize must not be negative")
	}
	bufSize := p.DictSize + p.ExtraBufSize
	if bufSize != int64(int(bufSize)) {
		return errors.New("buffer size too large for int")
	}
	return nil
}

// Default defines standard parameters.
//
// Use normalizeWriterExtraBufSize to set extra buf size to a reasonable
// value.
var Default = Parameters{
	LC:       3,
	LP:       0,
	PB:       2,
	DictSize: 8 * 1024 * 1024,
}