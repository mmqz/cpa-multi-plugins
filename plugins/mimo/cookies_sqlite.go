// cookies_sqlite.go — the pure-Go SQLite read of a copied Cookies store.
// modernc.org/sqlite keeps this cgo-free; the DB is a private temp copy, so
// no locking/WAL pragma gymnastics are needed beyond opening it plainly.
package main

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// queryCookieRows returns every row of the `cookies` table. Decryption and
// host filtering happen in cookies.go — this layer is intentionally dumb.
func queryCookieRows(dbPath string) ([]cookieRow, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name, value, encrypted_value, host_key, path, is_secure, is_httponly, expires_utc FROM cookies`)
	if err != nil {
		return nil, fmt.Errorf("query cookies: %w", err)
	}
	defer rows.Close()
	out := make([]cookieRow, 0, 64)
	for rows.Next() {
		var r cookieRow
		var value, encrypted []byte
		if err := rows.Scan(&r.Name, &value, &encrypted, &r.HostKey, &r.Path, &r.Secure, &r.HTTPOnly, &r.ExpiresUTC); err != nil {
			return nil, fmt.Errorf("scan cookie row: %w", err)
		}
		r.Value = string(value)
		r.Encrypted = encrypted
		out = append(out, r)
	}
	return out, rows.Err()
}
