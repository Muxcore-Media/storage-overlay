package overlay

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	streamMagic0 = 'M'
	streamMagic1 = 'X'
	streamMagic2 = 'O'
	streamFormat = byte(2)
	flagCompress = byte(1 << 0)
	flagEncrypt  = byte(1 << 1)

	plainChunkSize = 256 << 10 // 256 KiB plaintext per sealed chunk
)

func writeStreamHeader(w io.Writer, compress, encrypt bool) error {
	flags := byte(0)
	if compress {
		flags |= flagCompress
	}
	if encrypt {
		flags |= flagEncrypt
	}
	_, err := w.Write([]byte{streamMagic0, streamMagic1, streamMagic2, streamFormat, flags})
	return err
}

func readStreamHeader(r io.Reader) (compress, encrypt bool, err error) {
	hdr := make([]byte, 5)
	if _, err = io.ReadFull(r, hdr); err != nil {
		return false, false, err
	}
	if hdr[0] != streamMagic0 || hdr[1] != streamMagic1 || hdr[2] != streamMagic2 {
		return false, false, fmt.Errorf("invalid stream magic")
	}
	if hdr[3] != streamFormat {
		return false, false, fmt.Errorf("unsupported stream format %d", hdr[3])
	}
	return hdr[4]&flagCompress != 0, hdr[4]&flagEncrypt != 0, nil
}

// streamTransformOut writes compressed/encrypted payload. The returned hash is SHA-256
// of the pre-encryption payload (post-gzip), matching legacy dedup semantics.
func streamTransformOut(w io.Writer, cfg Config, plain io.Reader) (preEncryptHash []byte, plainSize int64, err error) {
	counted := &countReader{r: plain}
	preEncHash := sha256.New()

	if !cfg.Compress && !cfg.Encrypt {
		if _, err = io.Copy(io.MultiWriter(w, preEncHash), counted); err != nil {
			return nil, 0, err
		}
		return preEncHash.Sum(nil), counted.n, nil
	}

	if err = writeStreamHeader(w, cfg.Compress, cfg.Encrypt); err != nil {
		return nil, 0, err
	}

	var cw io.WriteCloser
	if cfg.Encrypt && cfg.Compress {
		pr, pw := io.Pipe()
		go func() {
			zw := gzip.NewWriter(io.MultiWriter(pw, preEncHash))
			_, copyErr := io.Copy(zw, counted)
			closeErr := zw.Close()
			_ = pw.CloseWithError(errors.Join(copyErr, closeErr))
		}()
		if err = writeEncryptedChunks(w, cfg.AESKey, pr); err != nil {
			return nil, 0, err
		}
		return preEncHash.Sum(nil), counted.n, nil
	}
	if cfg.Compress {
		zw := gzip.NewWriter(io.MultiWriter(w, preEncHash))
		cw = zw
	} else {
		cw = &nopCloserWriter{Writer: io.MultiWriter(w, preEncHash)}
	}

	if cfg.Encrypt {
		if err = writeEncryptedChunks(cw, cfg.AESKey, counted); err != nil {
			_ = cw.Close()
			return nil, 0, err
		}
	} else if cfg.Compress {
		buf := make([]byte, plainChunkSize)
		for {
			n, readErr := counted.Read(buf)
			if n > 0 {
				if _, err = cw.Write(buf[:n]); err != nil {
					_ = cw.Close()
					return nil, 0, err
				}
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				_ = cw.Close()
				return nil, 0, readErr
			}
		}
	} else {
		if _, err = io.Copy(cw, counted); err != nil {
			_ = cw.Close()
			return nil, 0, err
		}
	}
	if err = cw.Close(); err != nil {
		return nil, 0, err
	}
	return preEncHash.Sum(nil), counted.n, nil
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

type nopCloserWriter struct{ io.Writer }

func (n *nopCloserWriter) Close() error { return nil }

func writeEncryptedChunks(w io.Writer, key []byte, plain io.Reader) error {
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	buf := make([]byte, plainChunkSize)
	for {
		n, readErr := plain.Read(buf)
		if n > 0 {
			nonce := make([]byte, gcm.NonceSize())
			if _, err := rand.Read(nonce); err != nil {
				return err
			}
			sealed := gcm.Seal(nil, nonce, buf[:n], nil)
			var lenBuf [4]byte
			binary.BigEndian.PutUint32(lenBuf[:], uint32(len(sealed)))
			if _, err = w.Write(lenBuf[:]); err != nil {
				return err
			}
			if _, err = w.Write(nonce); err != nil {
				return err
			}
			if _, err = w.Write(sealed); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func streamTransformIn(r io.Reader, cfg Config) (io.ReadCloser, error) {
	peek := make([]byte, 5)
	n, err := io.ReadFull(r, peek)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	if n >= 4 && peek[0] == streamMagic0 && peek[1] == streamMagic1 && peek[2] == streamMagic2 {
		if n < 5 || peek[3] != streamFormat {
			return nil, fmt.Errorf("unsupported stream format")
		}
		compress := peek[4]&flagCompress != 0
		encrypt := peek[4]&flagEncrypt != 0
		if encrypt && compress {
			dec, err := newDecryptChunkReader(r, cfg.AESKey)
			if err != nil {
				return nil, err
			}
			zr, err := gzip.NewReader(dec)
			if err != nil {
				return nil, err
			}
			return zr, nil
		}
		var plain io.Reader = r
		if encrypt {
			plain, err = newDecryptChunkReader(r, cfg.AESKey)
			if err != nil {
				return nil, err
			}
		}
		if compress {
			zr, err := gzip.NewReader(plain)
			if err != nil {
				return nil, err
			}
			return zr, nil
		}
		return io.NopCloser(plain), nil
	}
	body := io.MultiReader(bytes.NewReader(peek[:n]), r)
	legacy, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	out, err := transformInLegacy(cfg, legacy)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(out)), nil
}

func transformInLegacy(cfg Config, blob []byte) ([]byte, error) {
	data := blob
	if cfg.Encrypt {
		out, err := open(cfg.AESKey, data)
		if err != nil {
			return nil, err
		}
		data = out
	}
	if cfg.Compress {
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		out, err := io.ReadAll(zr)
		_ = zr.Close()
		if err != nil {
			return nil, err
		}
		data = out
	}
	return data, nil
}

type decryptChunkReader struct {
	r    io.Reader
	gcm  cipher.AEAD
	buf  []byte
	off  int
	done bool
}

func newDecryptChunkReader(r io.Reader, key []byte) (*decryptChunkReader, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &decryptChunkReader{r: r, gcm: gcm}, nil
}

func (d *decryptChunkReader) Read(p []byte) (int, error) {
	for {
		if d.off < len(d.buf) {
			n := copy(p, d.buf[d.off:])
			d.off += n
			return n, nil
		}
		if d.done {
			return 0, io.EOF
		}
		var lenBuf [4]byte
		if _, err := io.ReadFull(d.r, lenBuf[:]); err != nil {
			if err == io.EOF {
				d.done = true
				return 0, io.EOF
			}
			return 0, err
		}
		sealedLen := binary.BigEndian.Uint32(lenBuf[:])
		if sealedLen == 0 {
			d.done = true
			return 0, io.EOF
		}
		nonce := make([]byte, d.gcm.NonceSize())
		if _, err := io.ReadFull(d.r, nonce); err != nil {
			return 0, err
		}
		sealed := make([]byte, sealedLen)
		if _, err := io.ReadFull(d.r, sealed); err != nil {
			return 0, err
		}
		plain, err := d.gcm.Open(nil, nonce, sealed, nil)
		if err != nil {
			return 0, err
		}
		d.buf = plain
		d.off = 0
	}
}

func (d *decryptChunkReader) Close() error { return nil }

type offsetLimitReader struct {
	src    io.Reader
	skip   int64
	remain int64
}

func (o *offsetLimitReader) Read(p []byte) (int, error) {
	for o.skip > 0 {
		buf := p
		if int64(len(buf)) > o.skip {
			buf = buf[:o.skip]
		}
		n, err := o.src.Read(buf)
		o.skip -= int64(n)
		if err != nil {
			return 0, err
		}
		if n == 0 {
			continue
		}
	}
	if o.remain == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > o.remain {
		p = p[:o.remain]
	}
	n, err := o.src.Read(p)
	o.remain -= int64(n)
	if err == io.EOF && o.remain > 0 {
		return n, io.EOF
	}
	return n, err
}
