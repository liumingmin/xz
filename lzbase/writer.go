package lzbase

import "io"

type Writer struct {
	OpCodec *OpCodec
	Dict    *WriterDict
	re      *rangeEncoder
	params  *Parameters
}

func InitWriter(bw *Writer, w io.Writer, oc *OpCodec, params Parameters) error {
	switch {
	case w == nil:
		return newError("InitWriter argument w is nil")
	case oc == nil:
		return newError("InitWriter argument oc is nil")
	}
	err := verifyParameters(&params)
	if err != nil {
		return err
	}
	dict, ok := oc.dict.(*WriterDict)
	if !ok {
		return newError("op codec for writer expected")
	}
	re := newRangeEncoder(w)
	*bw = Writer{OpCodec: oc, Dict: dict, re: re, params: &params}
	return nil
}

// Write moves data into the internal buffer and triggers its compression.
func (bw *Writer) Write(p []byte) (n int, err error) {
	end := bw.Dict.end + int64(len(p))
	if end < 0 {
		panic("end counter overflow")
	}
	var rerr error
	if bw.params.SizeInHeader && end > bw.params.Size {
		p = p[:bw.params.Size-end]
		rerr = newError("write exceeds unpackLen")
	}
	for n < len(p) {
		k, err := bw.Dict.Write(p[n:])
		n += k
		if err != nil && err != errAgain {
			return n, err
		}
		if err = bw.process(0); err != nil {
			return n, err
		}
	}
	return n, rerr
}

// Close terminates the LZMA stream. It doesn't close the underlying writer
// though and leaves it alone. In some scenarios explicit closing of the
// underlying writer is required.
func (bw *Writer) Close() error {
	var err error
	if err = bw.process(allData); err != nil {
		return err
	}
	if bw.params.EOS {
		if err = bw.writeEOS(); err != nil {
			return err
		}
	}
	if err = bw.re.Close(); err != nil {
		return err
	}
	return bw.Dict.Close()
}

// The allData flag tells the process method that all data must be processed.
const allData = 1

// indicates an empty buffer
var errEmptyBuf = newError("empty buffer")

// potentialOffsets creates a list of potential offsets for matches.
func (bw *Writer) potentialOffsets(p []byte) []int64 {
	oc := bw.OpCodec
	head := bw.Dict.Offset()
	start := bw.Dict.start
	offs := make([]int64, 0, 32)
	// add potential offsets with highest priority at the top
	for i := 1; i < 11; i++ {
		// distance 1 to 8
		off := head - int64(i)
		if start <= off {
			offs = append(offs, off)
		}
	}
	if len(p) == 4 {
		// distances from the hash table
		offs = append(offs, bw.Dict.Offsets(p)...)
	}
	for i := 3; i >= 0; i-- {
		// distances from the repetition for length less than 4
		dist := int64(oc.rep[i]) + minDistance
		off := head - dist
		if start <= off {
			offs = append(offs, off)
		}
	}
	return offs
}

// errNoMatch indicates that no match could be found
var errNoMatch = newError("no match found")

// bestMatch finds the best match for the given offsets.
//
// TODO: compare all possible commands for compressed bits per encoded bits.
func (bw *Writer) bestMatch(offsets []int64) (m match, err error) {
	oc := bw.OpCodec
	// creates a match for 1
	head := bw.Dict.Offset()
	off := int64(-1)
	length := 0
	for i := len(offsets) - 1; i >= 0; i-- {
		n := bw.Dict.EqualBytes(head, offsets[i], MaxLength)
		if n > length {
			off, length = offsets[i], n
		}
	}
	if off < 0 {
		err = errNoMatch
		return
	}
	if length == 1 {
		dist := int64(oc.rep[0]) + minDistance
		offRep0 := head - dist
		if off != offRep0 {
			err = errNoMatch
			return
		}
	}
	return match{distance: head - off, length: length}, nil
}

// findOp finds an operation for the head of the dictionary.
func (bw *Writer) findOp() (op operation, err error) {
	p := make([]byte, 4)
	n, err := bw.Dict.PeekHead(p)
	if err != nil && err != errAgain && err != io.EOF {
		return nil, err
	}
	if n <= 0 {
		if n < 0 {
			panic("strange n")
		}
		return nil, errEmptyBuf
	}
	offs := bw.potentialOffsets(p[:n])
	m, err := bw.bestMatch(offs)
	if err == errNoMatch {
		return lit{b: p[0]}, nil
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

// discardOp advances the head of the dictionary and writes the the bytes into
// the hash table.
func (bw *Writer) discardOp(op operation) error {
	n, err := bw.Dict.Copy(bw.Dict.t4, op.Len())
	if err != nil {
		return err
	}
	if n < op.Len() {
		return errAgain
	}
	return nil
}

// process encodes the data written into the dictionary buffer. The allData
// flag requires all data remaining in the buffer to be encoded.
func (bw *Writer) process(flags int) error {
	var lowMark int
	if flags&allData == 0 {
		lowMark = MaxLength - 1
	}
	for bw.Dict.Readable() > lowMark {
		op, err := bw.findOp()
		if err != nil {
			debug.Printf("findOp error %s\n", err)
			return err
		}
		if err = bw.writeOp(op); err != nil {
			return err
		}
		debug.Printf("op %s", op)
		if err = bw.discardOp(op); err != nil {
			return err
		}
	}
	return nil
}

// writeLiteral writes a literal into the operation stream
func (bw *Writer) writeLiteral(l lit) error {
	var err error
	oc := bw.OpCodec
	state, state2, _ := oc.states()
	if err = oc.isMatch[state2].Encode(bw.re, 0); err != nil {
		return err
	}
	litState := oc.litState()
	match := bw.Dict.Byte(int64(oc.rep[0]) + 1)
	err = oc.litCodec.Encode(bw.re, l.b, state, match, litState)
	if err != nil {
		return err
	}
	oc.updateStateLiteral()
	return nil
}

// writeEOS writes the explicit EOS marker
func (bw *Writer) writeEOS() error {
	return bw.writeMatch(match{distance: maxDistance, length: MinLength})
}

func iverson(ok bool) uint32 {
	if ok {
		return 1
	}
	return 0
}

// writeRep writes a repetition operation into the operation stream
func (bw *Writer) writeMatch(m match) error {
	var err error
	oc := bw.OpCodec
	if !(minDistance <= m.distance && m.distance <= maxDistance) {
		return newError("distance out of range")
	}
	dist := uint32(m.distance - minDistance)
	if !(MinLength <= m.length && m.length <= MaxLength) &&
		!(dist == oc.rep[0] && m.length == 1) {
		return newError("length out of range")
	}
	state, state2, posState := oc.states()
	if err = oc.isMatch[state2].Encode(bw.re, 1); err != nil {
		return err
	}
	var g int
	for g = 0; g < 4; g++ {
		if oc.rep[g] == dist {
			break
		}
	}
	b := iverson(g < 4)
	if err = oc.isRep[state].Encode(bw.re, b); err != nil {
		return err
	}
	n := uint32(m.length - MinLength)
	if b == 0 {
		// simple match
		oc.rep[3], oc.rep[2], oc.rep[1], oc.rep[0] = oc.rep[2],
			oc.rep[1], oc.rep[0], dist
		oc.updateStateMatch()
		if err = oc.lenCodec.Encode(bw.re, n, posState); err != nil {
			return err
		}
		return oc.distCodec.Encode(bw.re, dist, n)
	}
	b = iverson(g != 0)
	if err = oc.isRepG0[state].Encode(bw.re, b); err != nil {
		return err
	}
	if b == 0 {
		// g == 0
		b = iverson(m.length != 1)
		if err = oc.isRepG0Long[state2].Encode(bw.re, b); err != nil {
			return err
		}
		if b == 0 {
			oc.updateStateShortRep()
			return nil
		}
	} else {
		// g in {1,2,3}
		b = iverson(g != 1)
		if err = oc.isRepG1[state].Encode(bw.re, b); err != nil {
			return err
		}
		if b == 1 {
			// g in {2,3}
			b = iverson(g != 2)
			err = oc.isRepG2[state].Encode(bw.re, b)
			if err != nil {
				return err
			}
			if b == 1 {
				oc.rep[3] = oc.rep[2]
			}
			oc.rep[2] = oc.rep[1]
		}
		oc.rep[1] = oc.rep[0]
		oc.rep[0] = dist
	}
	oc.updateStateRep()
	return oc.repLenCodec.Encode(bw.re, n, posState)
}

// writeOp writes an operation value into the stream.
func (bw *Writer) writeOp(op operation) error {
	switch x := op.(type) {
	case match:
		return bw.writeMatch(x)
	case lit:
		return bw.writeLiteral(x)
	}
	panic("unknown operation type")
}
