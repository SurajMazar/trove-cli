package git

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/SurajMazar/trove-cli/internal/errs"
)

// SSHKey describes a private key found in ~/.ssh. Only public metadata is
// read (from the .pub file); the private key is never opened here.
type SSHKey struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	Type    string `json:"type,omitempty"`    // e.g. ssh-ed25519
	Comment string `json:"comment,omitempty"` // usually an email
}

// SSHDir returns ~/.ssh.
func SSHDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".ssh"
	}
	return filepath.Join(home, ".ssh")
}

// ListSSHKeys returns private keys in dir that have a matching .pub file.
func ListSSHKeys(dir string) ([]SSHKey, error) {
	pubs, err := filepath.Glob(filepath.Join(dir, "*.pub"))
	if err != nil {
		return nil, err
	}
	var out []SSHKey
	for _, pub := range pubs {
		priv := strings.TrimSuffix(pub, ".pub")
		if st, err := os.Stat(priv); err != nil || !st.Mode().IsRegular() {
			continue
		}
		k := SSHKey{Path: priv, Name: filepath.Base(priv)}
		if f, err := os.Open(pub); err == nil {
			sc := bufio.NewScanner(f)
			if sc.Scan() {
				fields := strings.Fields(sc.Text())
				if len(fields) >= 1 {
					k.Type = fields[0]
				}
				if len(fields) >= 3 {
					k.Comment = strings.Join(fields[2:], " ")
				}
			}
			f.Close()
		}
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ResolveSSHKey turns a user value into a validated private key path.
// Accepted forms: an absolute or ~-relative path, or a bare name looked up
// in ~/.ssh ("id_github"). A trailing ".pub" is stripped, because git needs
// the private half.
func ResolveSSHKey(spec string) (string, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", nil
	}
	path := spec
	switch {
	case path == "~" || strings.HasPrefix(path, "~/"):
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[1:])
		}
	case !strings.ContainsRune(path, filepath.Separator) && !strings.Contains(path, "/"):
		path = filepath.Join(SSHDir(), path)
	}
	path = strings.TrimSuffix(path, ".pub")
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", &errs.Error{Kind: errs.ErrInvalidArgument, Message: fmt.Sprintf("SSH key %s not found", abs),
			Hint: "ls ~/.ssh  (or create one: ssh-keygen -t ed25519)"}
	}
	if !st.Mode().IsRegular() {
		return "", errs.New(errs.ErrInvalidArgument, "SSH key %s is not a file", abs)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		return "", &errs.Error{Kind: errs.ErrInvalidArgument,
			Message: fmt.Sprintf("SSH key %s is readable by other users (%04o); ssh will refuse to use it", abs, st.Mode().Perm()),
			Hint:    "chmod 600 " + shellQuote(abs)}
	}
	return abs, nil
}
