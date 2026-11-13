package provider

import (
	"archive/tar"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"encoding/pem"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// readSecureArchive returns the files of the archive by name.
func readSecureArchive(t *testing.T, hostPort, admin, limited string) map[string]string {
	t.Helper()
	archive, err := secureClusterFiles(hostPort, admin, limited)
	require.NoError(t, err)
	files := map[string]string{}
	tr := tar.NewReader(archive)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files
		}
		require.NoError(t, err)
		data, err := io.ReadAll(tr)
		require.NoError(t, err)
		files[h.Name] = string(data)
	}
}

// The generated files do not need Docker, so a plain `go test` checks them.
func TestSecureClusterFiles(t *testing.T) {
	files := readSecureArchive(t, "12345", "adminpw", "limitedpw")

	for _, name := range []string{"secure/start.sh", "secure/server.properties", "secure/server.pem", "secure/cert.pem"} {
		require.Contains(t, files, name)
	}

	t.Run("properties", func(t *testing.T) {
		props := files["secure/server.properties"]
		require.Contains(t, props, "advertised.listeners=SASL_SSL://localhost:12345")
		require.Contains(t, props, "allow.everyone.if.no.acl.found=false")
		require.Contains(t, props, `user_limited="limitedpw"`)
		require.NotContains(t, props, "super.users=User:limited")
	})

	t.Run("scram_credentials_for_both_users_and_mechanisms", func(t *testing.T) {
		script := files["secure/start.sh"]
		for _, user := range []string{"admin", "limited"} {
			for _, mech := range []string{"SCRAM-SHA-256", "SCRAM-SHA-512"} {
				require.Contains(t, script, "--add-scram '"+mech+"=[name="+user+",")
			}
		}
	})

	t.Run("certificate_is_self_signed_and_matches_key", func(t *testing.T) {
		pair, err := tls.X509KeyPair([]byte(files["secure/cert.pem"]), []byte(files["secure/server.pem"]))
		require.NoError(t, err, "the key in server.pem must match cert.pem")

		block, _ := pem.Decode([]byte(files["secure/cert.pem"]))
		cert, err := x509.ParseCertificate(block.Bytes)
		require.NoError(t, err)
		require.NoError(t, cert.VerifyHostname("localhost"))
		require.NoError(t, cert.VerifyHostname("127.0.0.1"))

		_, err = cert.Verify(x509.VerifyOptions{})
		require.Error(t, err, "a client that checks the certificate must reject it")
		require.NotEmpty(t, pair.Certificate)
	})

	t.Run("each_run_makes_a_new_key", func(t *testing.T) {
		other := readSecureArchive(t, "12345", "adminpw", "limitedpw")
		require.NotEqual(t, strings.TrimSpace(files["secure/server.pem"]), strings.TrimSpace(other["secure/server.pem"]))
	})
}
