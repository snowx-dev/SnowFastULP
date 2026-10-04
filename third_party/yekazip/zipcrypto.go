package zip

import (
	"hash/crc32"
	"io"
)

type ZipCrypto struct {
	password []byte
	Keys     [3]uint32
}

func NewZipCrypto(passphrase []byte) *ZipCrypto {
	z := &ZipCrypto{}
	z.password = passphrase
	z.init()
	return z
}

func (z *ZipCrypto) init() {
	z.Keys[0] = 0x12345678
	z.Keys[1] = 0x23456789
	z.Keys[2] = 0x34567890

	for i := 0; i < len(z.password); i++ {
		z.updateKeys(z.password[i])
	}
}

func (z *ZipCrypto) updateKeys(byteValue byte) {
	z.Keys[0] = crc32update(z.Keys[0], byteValue)
	z.Keys[1] += z.Keys[0] & 0xff
	z.Keys[1] = z.Keys[1]*134775813 + 1
	z.Keys[2] = crc32update(z.Keys[2], (byte)(z.Keys[1]>>24))
}

func (z *ZipCrypto) magicByte() byte {
	var t uint32 = z.Keys[2] | 2
	return byte((t * (t ^ 1)) >> 8)
}

func (z *ZipCrypto) Encrypt(data []byte) []byte {
	length := len(data)
	chiper := make([]byte, length)
	for i := 0; i < length; i++ {
		v := data[i]
		chiper[i] = v ^ z.magicByte()
		z.updateKeys(v)
	}
	return chiper
}

func (z *ZipCrypto) Decrypt(chiper []byte) []byte {
	plain := make([]byte, len(chiper))
	copy(plain, chiper)
	z.decryptInPlace(plain)
	return plain
}

// decryptInPlace decrypts b in place, advancing the key state. The streaming
// decryptor uses it so memory stays O(caller buffer) regardless of the
// declared compressed size.
func (z *ZipCrypto) decryptInPlace(b []byte) {
	for i, c := range b {
		v := c ^ z.magicByte()
		z.updateKeys(v)
		b[i] = v
	}
}

func crc32update(pCrc32 uint32, bval byte) uint32 {
	return crc32.IEEETable[(pCrc32^uint32(bval))&0xff] ^ (pCrc32 >> 8)
}

// zipCryptoReader decrypts a legacy ZipCrypto member stream. The 12-byte
// encryption header is decrypted once on the first Read; payload bytes are
// decrypted in place into the caller's buffer while the key state advances, so
// memory is O(read buffer) independent of the declared compressed size.
type zipCryptoReader struct {
	r          *io.SectionReader
	z          *ZipCrypto
	headerRead bool
	remaining  int64 // payload bytes left = declared size - 12-byte header
	err        error // sticky
}

func (zr *zipCryptoReader) Read(p []byte) (int, error) {
	if zr.err != nil {
		return 0, zr.err
	}
	if !zr.headerRead {
		var hdr [zipCryptoHeaderLen]byte
		if _, err := io.ReadFull(zr.r, hdr[:]); err != nil {
			zr.err = err
			return 0, zr.err
		}
		zr.z.decryptInPlace(hdr[:])
		zr.headerRead = true
		zr.remaining = zr.r.Size() - zipCryptoHeaderLen
	}
	if zr.remaining <= 0 {
		zr.err = io.EOF
		return 0, io.EOF
	}
	if int64(len(p)) > zr.remaining {
		p = p[:zr.remaining]
	}
	n, err := zr.r.Read(p)
	zr.z.decryptInPlace(p[:n])
	zr.remaining -= int64(n)
	switch {
	case err == io.EOF && zr.remaining > 0:
		// Declared size promises more payload than the reader provides.
		err = io.ErrUnexpectedEOF
	case err == nil && zr.remaining == 0:
		err = io.EOF
	}
	if err != nil {
		zr.err = err
	}
	return n, err
}

// ZipCryptoDecryptor wraps the encrypted member stream (12-byte encryption
// header followed by the encrypted payload) in a streaming decryptor. The
// declared size must at least cover the encryption header; payload bytes are
// only ever decrypted into caller buffers, never buffered whole.
func ZipCryptoDecryptor(r *io.SectionReader, password []byte) (io.Reader, error) {
	if r.Size() < zipCryptoHeaderLen {
		return nil, ErrFormat
	}
	return &zipCryptoReader{r: r, z: NewZipCrypto(password)}, nil
}

type zipCryptoWriter struct {
	w     io.Writer
	z     *ZipCrypto
	first bool
	fw    *fileWriter
}

func (z *zipCryptoWriter) Write(p []byte) (n int, err error) {
	err = nil
	if z.first {
		z.first = false
		header := []byte{0xF8, 0x53, 0xCF, 0x05, 0x2D, 0xDD, 0xAD, 0xC8, 0x66, 0x3F, 0x8C, 0xAC}
		header = z.z.Encrypt(header)

		crc := z.fw.ModifiedTime
		header[10] = byte(crc)
		header[11] = byte(crc >> 8)

		z.z.init()
		z.w.Write(z.z.Encrypt(header))
		n += 12
	}
	z.w.Write(z.z.Encrypt(p))
	return
}

func ZipCryptoEncryptor(i io.Writer, pass passwordFn, fw *fileWriter) (io.Writer, error) {
	z := NewZipCrypto(pass())
	zc := &zipCryptoWriter{i, z, true, fw}
	return zc, nil
}
