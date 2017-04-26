package rmarsh

import (
	"encoding/binary"
	"io"
	"math/big"
	"strconv"

	"github.com/pkg/errors"
)

type Token uint8

const (
	tokenStart = iota
	TokenNil
	TokenTrue
	TokenFalse
	TokenFixnum
	TokenFloat
	TokenBigNum
	TokenEOF
)

var tokenNames = map[Token]string{
	TokenNil:    "TokenNil",
	TokenTrue:   "TokenTrue",
	TokenFalse:  "TokenFalse",
	TokenFixnum: "TokenFixnum",
	TokenFloat:  "TokenFloat",
	TokenBigNum: "TokenBigNum",
	TokenEOF:    "EOF",
}

func (t Token) String() string {
	if n, ok := tokenNames[t]; ok {
		return n
	}
	return "UNKNOWN"
}

type Parser struct {
	r   io.Reader
	cur Token
	pos uint64

	buf      []byte
	ctx      []byte
	num      int64
	flt      *float64
	bnum     *big.Int
	bnumsign byte
}

// NewParser constructs a new parser that streams data from the given io.Reader
// Due to the nature of the Marshal format, data is read in very small increments
// please ensure that the provided Reader is buffered, or wrap it in a bufio.Reader.
func NewParser(r io.Reader) *Parser {
	return &Parser{r: r, buf: make([]byte, 64)}
}

func (p *Parser) Reset() {
	p.pos = 0
	p.cur = tokenStart
}

// Next advances the parser to the next token in the stream.
func (p *Parser) Next() (Token, error) {
	if err := p.adv(); err != nil {
		return 0, errors.Wrap(err, "rmarsh.Parser.Next()")
	}
	return p.cur, nil
}

// Int returns the value contained in the current Fixnum token.
// Returns an error if called for any other type of token.
func (p *Parser) Int() (int64, error) {
	if p.cur != TokenFixnum {
		return 0, errors.Errorf("rmarsh.Parser.Int() called for wrong token: %s", p.cur)
	}
	return p.num, nil
}

// Float returns the value contained in the current Float token.
// Returns an error if called for any other type of token.
func (p *Parser) Float() (float64, error) {
	if p.cur != TokenFloat {
		return 0, errors.Errorf("rmarsh.Parser.Float() called for wrong token: %s", p.cur)
	}
	if p.flt == nil {
		if flt, err := strconv.ParseFloat(string(p.ctx), 64); err != nil {
			return 0, errors.Wrap(err, "rmarsh.Parser.Float()")
		} else {
			p.flt = &flt
		}
	}
	return *p.flt, nil
}

// BigNum returns the value contained in the current BigNum token.
// Returns an error if called for any other type of token.
func (p *Parser) BigNum() (*big.Int, error) {
	if p.cur != TokenBigNum {
		return nil, errors.Errorf("rmarsh.Parser.BigNum() called for wrong token: %s", p.cur)
	}
	if p.bnum == nil {
		reverseBytes(p.ctx)
		p.bnum = new(big.Int).SetBytes(p.ctx)
		if p.bnumsign == '-' {
			p.bnum = p.bnum.Neg(p.bnum)
		}
	}
	return p.bnum, nil
}

func (p *Parser) adv() (err error) {
	var typ byte

	if p.cur == tokenStart {
		if b, err := p.readbytes(3); err != nil {
			return errors.Wrap(err, "reading magic")
		} else if b[0] != 0x04 || b[1] != 0x08 {
			return errors.Errorf("Expected magic header 0x0408, got 0x%.4X", binary.BigEndian.Uint16(magic))
		} else {
			// Silly little optimisation: we fetched 3 bytes on the first
			// read since there is always at least one token to read.
			// Saves a couple dozen nanos on them micro benchmarks. #winning #tigerblood
			typ = b[2]
		}
	} else {
		typ, err = p.readbyte()
		if err == io.EOF {
			p.cur = TokenEOF
			return nil
		} else if err != nil {
			return errors.Wrap(err, "read type id")
		}
	}

	switch typ {
	case TYPE_NIL:
		p.cur = TokenNil
	case TYPE_TRUE:
		p.cur = TokenTrue
	case TYPE_FALSE:
		p.cur = TokenFalse
	case TYPE_FIXNUM:
		p.cur = TokenFixnum
		p.num, err = p.long()
		if err != nil {
			return errors.Wrap(err, "fixnum")
		}
	case TYPE_FLOAT:
		p.cur = TokenFloat
		// Float() caches the result of strconv.ParseFloat, since it's pretty expensive.
		// We clear out any previously cached value if we've parsed a float earlier in
		// the stream.
		p.flt = nil
		if err := p.sizedBlob(false); err != nil {
			return errors.Wrap(err, "float")
		}
	case TYPE_BIGNUM:
		p.cur = TokenBigNum
		p.bnum = nil
		p.bnumsign, err = p.readbyte()
		if err != nil {
			return errors.Wrap(err, "bignum")
		}

		if err := p.sizedBlob(true); err != nil {
			return errors.Wrap(err, "bignum")
		}
	}

	return nil
}

// Strings, Symbols, Floats, Bignums and the like all begin with an encoded long
// for the size and then the raw bytes. In most cases, interpreting those bytes
// is relatively expensive - and the caller may not even care (just skips to the
// next token). So, we save off the raw bytes and interpret them only when needed.
func (p *Parser) sizedBlob(bnum bool) error {
	sz, err := p.long()
	if err != nil {
		return err
	}

	// For some stupid reason bignums store the length in shorts, not bytes.
	if bnum {
		sz = sz * 2
	}

	p.ctx, err = p.readbytes(uint64(sz))
	return err
}

func (p *Parser) long() (int64, error) {
	b, err := p.readbyte()
	if err != nil {
		return 0, err
	}

	c := int(int8(b))
	if c == 0 {
		return 0, nil
	}

	if c > 0 {
		if 4 < c && c < 128 {
			return int64(c - 5), nil
		}

		raw, err := p.readbytes(uint64(c))
		if err != nil {
			return 0, err
		}
		bytes := make([]byte, 8)
		for i, v := range raw {
			bytes[i] = v
		}
		return int64(binary.LittleEndian.Uint64(bytes)), nil
	}

	if -129 < c && c < -4 {
		return int64(c + 5), nil
	}

	c = -c
	raw, err := p.readbytes(uint64(c))
	if err != nil {
		return 0, err
	}
	x := int64(-1)
	for i, v := range raw {
		x &= ^(int64(0xff) << uint(8*i))
		x |= int64(v) << uint(8*i)
	}

	return x, err
}

func (p *Parser) readbyte() (byte, error) {
	if buf, err := p.readbytes(1); err != nil {
		return 0, err
	} else {
		return buf[0], nil
	}
}

func (p *Parser) readbytes(num uint64) ([]byte, error) {
	if uint64(cap(p.buf)) < num {
		p.buf = make([]byte, num)
	}
	b := p.buf[:num]
	if _, err := io.ReadFull(p.r, b); err == io.EOF {
		return nil, err
	} else if err != nil {
		return nil, errors.Errorf("I/O error %q at position %d", err, p.pos)
	}
	p.pos += num
	return b, nil
}