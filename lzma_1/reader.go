package lzma_1

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/ulikunitz/lz"
)

// Properties define the properties for the LZMA and LZMA2 compression.
type Properties struct {
	LC int
	LP int
	PB int
}

// Returns the byte that encodes the properties.
func (p Properties) byte() byte {
	return (byte)((p.PB*5+p.LP)*9 + p.LC)
}

func (p *Properties) fromByte(b byte) error {
	p.LC = int(b % 9)
	b /= 9
	p.LP = int(b % 5)
	b /= 5
	p.PB = int(b)
	if p.PB > 4 {
		return errors.New("lzma: invalid properties byte")
	}
	return nil
}

func (p Properties) Verify() error {
	if !(0 <= p.LC && p.LC <= 8) {
		return fmt.Errorf("lzma: LC out of range 0..8")
	}
	if !(0 <= p.LP && p.LP <= 4) {
		return fmt.Errorf("lzma: LP out of range 0..4")
	}
	if !(0 <= p.PB && p.PB <= 4) {
		return fmt.Errorf("lzma: PB out of range 0..4")
	}
	return nil
}

// eosSize is used for the uncompressed size if it is unknown
const eosSize uint64 = 0xffffffffffffffff

// headerLen defines the length of an LZMA header
const headerLen = 13

// params defines the parameters for the LZMA method
type params struct {
	props            Properties
	dictSize         uint32
	uncompressedSize uint64
}

func (h params) Verify() error {
	if uint64(h.dictSize) > math.MaxInt {
		return errors.New("lzma: dictSize exceed max integer")
	}
	if h.dictSize < minDictSize {
		return errors.New("lzma: dictSize is too small")
	}
	return h.props.Verify()
}

// append adds the header to the slice s.
func (h params) AppendBinary(p []byte) (r []byte, err error) {
	var a [headerLen]byte
	a[0] = h.props.byte()
	putLE32(a[1:], h.dictSize)
	putLE64(a[5:], h.uncompressedSize)
	return append(p, a[:]...), nil
}

// parse parses the header from the slice x. x must have exactly header length.
func (h *params) UnmarshalBinary(x []byte) error {
	if len(x) != headerLen {
		return errors.New("lzma: LZMA header has incorrect length")
	}
	var err error
	if err = h.props.fromByte(x[0]); err != nil {
		return err
	}
	h.dictSize = getLE32(x[1:])
	h.uncompressedSize = getLE64(x[5:])
	return nil
}

// reader decompresses a byte stream of LZMA data.
type reader struct {
	dict              lz.Buffer
	state             state
	rd                rangeDecoder
	uncompressedLimit int64
	err               error
}

func (r *reader) start(z io.Reader, uncompressedSize uint64) error {
	if uncompressedSize == eosSize {
		r.uncompressedLimit = -1
	} else {
		u := uint64(r.dict.Pos()) + uncompressedSize
		r.uncompressedLimit = int64(u)
		if r.uncompressedLimit < 0 || u < uncompressedSize {
			return errors.New("lzma: overflow")
		}
	}
	br, ok := z.(io.ByteReader)
	if !ok {
		br = bufio.NewReader(z)
	}
	if err := r.rd.init(br); err != nil {
		return err
	}
	r.err = nil
	return nil
}

func (r *reader) decodeLiteral() (seq lz.Seq, err error) {
	litState := r.state.litState(r.dict.ByteAtEnd(1), r.dict.Pos())
	match := r.dict.ByteAtEnd(int(r.state.rep[0]) + 1)
	s, err := r.state.litCodec.Decode(&r.rd, r.state.state, match, litState)
	if err != nil {
		return lz.Seq{}, err
	}
	return lz.Seq{LitLen: 1, Aux: uint32(s)}, nil
}

var errEOS = errors.New("EOS marker")

// Distance for EOS marker
const eosDist = 1<<32 - 1

// readSeq reads a single sequence. We are encoding a little bit differently
// than normal, because each seq is either a one-byte literal (LitLen=1, AUX has
// the byte) or a match (MatchLen and Offset non-zero).
func (r *reader) readSeq() (seq lz.Seq, err error) {
	state, state2, posState := r.state.states(r.dict.Pos())

	s2 := &r.state.s2[state2]
	b, err := r.rd.decodeBit(&s2.isMatch)
	if err != nil {
		return lz.Seq{}, err
	}
	if b == 0 {
		// literal
		seq, err := r.decodeLiteral()
		if err != nil {
			return lz.Seq{}, err
		}
		r.state.updateStateLiteral()
		return seq, nil
	}

	s1 := &r.state.s1[state]
	b, err = r.rd.decodeBit(&s1.isRep)
	if err != nil {
		return lz.Seq{}, err
	}
	if b == 0 {
		// simple match
		r.state.rep[3], r.state.rep[2], r.state.rep[1] =
			r.state.rep[2], r.state.rep[1], r.state.rep[0]

		r.state.updateStateMatch()
		// The length decoder returns the length offset.
		n, err := r.state.lenCodec.Decode(&r.rd, posState)
		if err != nil {
			return lz.Seq{}, err
		}
		// The dist decoder returns the distance offset. The actual
		// distance is 1 higher.
		r.state.rep[0], err = r.state.distCodec.Decode(&r.rd, n)
		if err != nil {
			return lz.Seq{}, err
		}
		if r.state.rep[0] == eosDist {
			return lz.Seq{}, errEOS
		}
		return lz.Seq{MatchLen: n + minMatchLen,
			Offset: r.state.rep[0] + minDistance}, nil
	}
	b, err = r.rd.decodeBit(&s1.isRepG0)
	if err != nil {
		return lz.Seq{}, err
	}
	dist := r.state.rep[0]
	if b == 0 {
		// rep match 0
		b, err = r.rd.decodeBit(&s2.isRepG0Long)
		if err != nil {
			return lz.Seq{}, err
		}
		if b == 0 {
			r.state.updateStateShortRep()
			return lz.Seq{MatchLen: 1, Offset: dist + minDistance},
				nil
		}
	} else {
		b, err = r.rd.decodeBit(&s1.isRepG1)
		if err != nil {
			return lz.Seq{}, err
		}
		if b == 0 {
			dist = r.state.rep[1]
		} else {
			b, err = r.rd.decodeBit(&s1.isRepG2)
			if err != nil {
				return lz.Seq{}, err
			}
			if b == 0 {
				dist = r.state.rep[2]
			} else {
				dist = r.state.rep[3]
				r.state.rep[3] = r.state.rep[2]
			}
			r.state.rep[2] = r.state.rep[1]
		}
		r.state.rep[1] = r.state.rep[0]
		r.state.rep[0] = dist
	}
	n, err := r.state.repLenCodec.Decode(&r.rd, posState)
	if err != nil {
		return lz.Seq{}, err
	}
	r.state.updateStateRep()
	return lz.Seq{MatchLen: n + minMatchLen, Offset: dist + minDistance},
		nil
}

func (r *reader) fillBuffer() error {
	if r.err != nil {
		return r.err
	}
	for r.dict.Available() >= maxMatchLen {
		seq, err := r.readSeq()
		if err != nil {
			s := r.uncompressedLimit
			switch err {
			case errEOS:
				if r.rd.possiblyAtEnd() && (s < 0 || s == r.dict.Pos()) {
					err = io.EOF
				}
			case io.EOF:
				if !r.rd.possiblyAtEnd() || s != r.dict.Pos() {
					err = io.ErrUnexpectedEOF
				}
			}
			r.err = err
			return err
		}
		if seq.MatchLen == 0 {
			if err = r.dict.WriteByte(byte(seq.Aux)); err != nil {
				panic(err)
			}
		} else {
			err = r.dict.WriteMatch(int(seq.MatchLen),
				int(seq.Offset))
			if err != nil {
				r.err = err
				return err
			}
		}
		if r.uncompressedLimit == r.dict.Pos() {
			err = io.EOF
			if !r.rd.possiblyAtEnd() {
				_, serr := r.readSeq()
				if serr != errEOS || !r.rd.possiblyAtEnd() {
					err = ErrEncoding
				}
			}
			r.err = err
			return err
		}
	}
	return nil
}

func (r *reader) Read(p []byte) (n int, err error) {
	for {
		// Read from a dictionary never returns an error
		k, _ := r.dict.Read(p[n:])
		n += k
		if n == len(p) {
			return n, nil
		}
		if err = r.fillBuffer(); err != nil {
			if r.dict.Len() > 0 {
				continue
			}
			return n, err
		}
	}
}

type uncompressedReader struct {
	dict *lz.Buffer
	z    io.Reader
}

func (r *uncompressedReader) fillBuffer() error {
	_, err := io.CopyN(r.dict, r.z, int64(r.dict.Available()))
	return err
}

func (r *uncompressedReader) Read(p []byte) (n int, err error) {
	for {
		// dictionary reads never return an error
		k, _ := r.dict.Read(p[n:])
		n += k
		if n == len(p) {
			return n, nil
		}
		if err = r.fillBuffer(); err != nil {
			if r.dict.Len() > 0 {
				continue
			}
			return n, err
		}
	}
}