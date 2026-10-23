// Certificate pinning: connect to a Quack server over HTTPS by pinning the
// SHA-256 fingerprint of its (usually self-signed) certificate with the
// ssl_fingerprint DSN parameter. A pin replaces CA and hostname
// verification: exactly the certificate with that fingerprint is accepted.
// It also switches the DSN to HTTPS, even on localhost.
//
// Start an HTTPS server in the DuckDB 2.0 CLI (quack_generate_keys prints
// the fingerprint; it is a no-op when the directory already has keys):
//
//	LOAD quack;
//	SELECT fingerprint FROM quack_generate_keys(directory := '/tmp/quack-keys');
//	CALL quack_serve('quack:127.0.0.1:9495', token => 'super_secret', disable_ssl => false,
//	    ssl_cert_file => '/tmp/quack-keys/server.pem',
//	    ssl_key_file => '/tmp/quack-keys/private_key.pem');
//
// then pass the fingerprint, or the certificate to compute it from:
//
//	QUACK_PORT=9495 QUACK_SSL_FINGERPRINT=7C:B3:...:7B go run ./examples/tls-pin
//	QUACK_PORT=9495 QUACK_CERT_FILE=/tmp/quack-keys/server.pem go run ./examples/tls-pin
//
// Without either variable the program prints these instructions and exits.
// The server address comes from QUACK_HOST, QUACK_PORT and QUACK_TOKEN
// (default 127.0.0.1:9494, token super_secret).
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	_ "github.com/zzir/quackdriver"
)

func main() {
	host := env("QUACK_HOST", "127.0.0.1")
	port := env("QUACK_PORT", "9494")
	token := env("QUACK_TOKEN", "super_secret")

	pin := os.Getenv("QUACK_SSL_FINGERPRINT")
	if pin == "" {
		if file := os.Getenv("QUACK_CERT_FILE"); file != "" {
			var err error
			if pin, err = certFingerprint(file); err != nil {
				log.Fatal(err)
			}
			fmt.Printf("fingerprint of %s: %s\n", file, pin)
		}
	}
	if pin == "" {
		usage(port)
		return
	}

	dsn := func(params ...string) string {
		q := append([]string{"token=" + url.QueryEscape(token)}, params...)
		return "quack://" + net.JoinHostPort(host, port) + "?" + strings.Join(q, "&")
	}
	ctx := context.Background()

	// 1. The pin: any spelling works (colons, "sha256:" prefix, any case);
	// URL-encode it since ':' separators are common.
	db, err := sql.Open("quack", dsn("ssl_fingerprint="+url.QueryEscape(pin)))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	var answer int
	if err := db.QueryRowContext(ctx, "SELECT 40 + 2").Scan(&answer); err != nil {
		log.Fatalf("pinned connection: %v", err)
	}
	fmt.Printf("pinned connection works: SELECT 40 + 2 = %d\n", answer)

	// The server reports the fingerprint of the certificate it serves.
	var served sql.NullString
	if err := db.QueryRowContext(ctx,
		"SELECT info['ssl_fingerprint'] FROM quack_server_list() WHERE port = ?", port).Scan(&served); err == nil {
		fmt.Printf("server certificate: %s (matches pin: %v)\n", served.String, normalize(served.String) == normalize(pin))
	}

	// 2. A different pin is refused before any request reaches the server.
	if err := tryConnect(ctx, dsn("ssl_fingerprint="+wrongPin(pin))); err != nil {
		fmt.Printf("wrong pin refused: %v\n", err)
	} else {
		log.Fatal("a wrong pin was accepted")
	}

	// 3. Plain HTTPS without a pin verifies the certificate against the
	// system CAs, which a self-signed certificate fails.
	if err := tryConnect(ctx, dsn("tls=true")); err != nil {
		fmt.Printf("unpinned HTTPS refused: %v\n", err)
	} else {
		fmt.Println("unpinned HTTPS accepted (the certificate verifies against the system CAs)")
	}

	// 4. A pin cannot be combined with plain HTTP.
	if err := tryConnect(ctx, dsn("tls=false", "ssl_fingerprint="+url.QueryEscape(pin))); err != nil {
		fmt.Printf("pin with tls=false rejected: %v\n", err)
	}
}

func tryConnect(ctx context.Context, dsn string) error {
	db, err := sql.Open("quack", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return db.PingContext(ctx)
}

// certFingerprint returns the SHA-256 of the first certificate in a PEM
// file, the value ssl_fingerprint pins.
func certFingerprint(file string) (string, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return "", fmt.Errorf("%s: no PEM certificate found", file)
		}
		if block.Type == "CERTIFICATE" {
			sum := sha256.Sum256(block.Bytes)
			return strings.ToUpper(hex.EncodeToString(sum[:])), nil
		}
	}
}

// normalize reduces a fingerprint to its 64 uppercase hex digits.
func normalize(fp string) string {
	fp = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(fp)), "sha256:")
	return strings.ToUpper(strings.NewReplacer(":", "", " ", "").Replace(fp))
}

// wrongPin returns pin with its last hex digit changed.
func wrongPin(pin string) string {
	p := normalize(pin)
	last := "0"
	if strings.HasSuffix(p, "0") {
		last = "1"
	}
	return p[:len(p)-1] + last
}

func usage(port string) {
	fmt.Printf(`No server certificate given: set QUACK_SSL_FINGERPRINT (or QUACK_CERT_FILE).

Start an HTTPS Quack server in the DuckDB 2.0 CLI, for example:

  LOAD quack;
  SELECT fingerprint FROM quack_generate_keys(directory := '/tmp/quack-keys');
  CALL quack_serve('quack:127.0.0.1:%[1]s', token => 'super_secret', disable_ssl => false,
      ssl_cert_file => '/tmp/quack-keys/server.pem',
      ssl_key_file => '/tmp/quack-keys/private_key.pem');

then run:

  QUACK_PORT=%[1]s QUACK_SSL_FINGERPRINT=<fingerprint> go run ./examples/tls-pin

A running server also reports its fingerprint in quack_server_list():
  SELECT info['ssl_fingerprint'] FROM quack_server_list();
`, port)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
