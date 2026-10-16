package deploy

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// scramIterations is Postgres' own default (scram_iterations).
const scramIterations = 4096

// scramVerifier returns the SCRAM-SHA-256 verifier Postgres would store for
// password -- "SCRAM-SHA-256$<iterations>:<salt>$<StoredKey>:<ServerKey>",
// all base64 -- or false when password is not plain ASCII.
//
// Handing Postgres the verifier instead of the password (`PASSWORD
// 'SCRAM-SHA-256$...'`) sets the very same password: Postgres stores a
// verifier given to it as is, whatever password_encryption says, and a
// client logs in with the password as usual (an md5 line in pg_hba.conf
// uses SCRAM too for such a role). What it buys: the password never appears
// in the SQL, so not in the file, not in the server log (a failed statement
// is logged in full by default; log_statement=ddl logs every one), not in
// psql's echo. A verifier does not log anyone in by itself.
//
// Postgres runs the password through SASLprep before hashing it; for ASCII
// that changes nothing (printable characters are kept, a control character
// makes it use the password unchanged), for anything else it may, so a
// non-ASCII password is left to the caller (false).
func scramVerifier(password string) (string, bool, error) {
	for i := 0; i < len(password); i++ {
		if password[i] >= 0x80 {
			return "", false, nil
		}
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", false, fmt.Errorf("couldn't generate a salt: %w", err)
	}
	v, err := scramVerifierWithSalt(password, salt, scramIterations)
	return v, err == nil, err
}

func scramVerifierWithSalt(password string, salt []byte, iterations int) (string, error) {
	salted, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		return "", err
	}
	clientKey := hmacSHA256(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	serverKey := hmacSHA256(salted, "Server Key")
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iterations, b64(salt), b64(storedKey[:]), b64(serverKey)), nil
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}
