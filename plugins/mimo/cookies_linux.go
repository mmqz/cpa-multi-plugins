//go:build !windows && !darwin

// cookies_linux.go — Linux os_crypt: v10/v11 cookies are AES-128-CBC under
// the fixed PBKDF2 password Chromium ships on Linux ("peanuts"/"saltysalt",
// 1 iteration, 16-byte key, IV = 16 space bytes). No Local State involved.
package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/binary"
)

func decryptPlatformV10(payload []byte, userDataDir, version string) ([]byte, error) {
	key := pbkdf2SHA1([]byte("peanuts"), []byte("saltysalt"), 1, 16)
	return aesCBCDecrypt(key, payload)
}

// pbkdf2SHA1 is an inline PBKDF2 (RFC 8018, HMAC-SHA1) so the plugin avoids
// pulling golang.org/x/crypto for 20 lines. iter=1 makes this trivially fast.
func pbkdf2SHA1(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha1.New, password)
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen
	var buf [4]byte
	dk := make([]byte, 0, numBlocks*hashLen)
	U := make([]byte, hashLen)
	for block := 1; block <= numBlocks; block++ {
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(buf[:], uint32(block))
		prf.Write(buf[:4])
		dk = prf.Sum(dk)
		T := dk[len(dk)-hashLen:]
		copy(U, T)
		for n := 2; n <= iter; n++ {
			prf.Reset()
			prf.Write(U)
			U = U[:0]
			U = prf.Sum(U)
			for x := range U {
				T[x] ^= U[x]
			}
		}
	}
	return dk[:keyLen]
}
