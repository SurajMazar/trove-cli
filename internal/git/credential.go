package git

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// HelperCommand builds a credential.helper value that runs exe with args
// through git's shell ("!cmd"). Arguments are single-quoted so paths with
// spaces work.
func HelperCommand(exe string, args ...string) string {
	parts := []string{shellQuote(exe)}
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	return "!" + strings.Join(parts, " ")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// CredentialRequest is the attribute set git sends to a credential helper.
type CredentialRequest struct {
	Protocol string
	Host     string
	Path     string
	Username string
}

// ReadCredentialRequest parses git's credential helper input
// (key=value lines terminated by a blank line or EOF).
func ReadCredentialRequest(r io.Reader) (CredentialRequest, error) {
	var req CredentialRequest
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "protocol":
			req.Protocol = v
		case "host":
			req.Host = v
		case "path":
			req.Path = v
		case "username":
			req.Username = v
		}
	}
	return req, sc.Err()
}

// WriteCredential writes a credential helper response.
func WriteCredential(w io.Writer, username, password string) error {
	if strings.ContainsAny(username, "\n\x00") || strings.ContainsAny(password, "\n\x00") {
		return fmt.Errorf("credential contains invalid characters")
	}
	_, err := fmt.Fprintf(w, "username=%s\npassword=%s\n\n", username, password)
	return err
}
