package bitwarden

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
)

// call records one CLI invocation.
type call struct {
	name  string
	args  []string
	stdin []byte
}

// fakeBW emulates the parts of the bw CLI Trove uses, backed by an in-memory
// vault. It decodes the base64 JSON that create/edit read from stdin exactly
// like bw does.
type fakeBW struct {
	mu           sync.Mutex
	status       string // unlocked | locked | unauthenticated
	notInstalled bool
	lockAfter    int // when >0, report "Vault is locked." once this many calls have run
	items        map[string]map[string]any
	trash        map[string]map[string]any
	folders      map[string]string // id -> name
	next         int
	calls        []call
}

func newFakeBW() *fakeBW {
	return &fakeBW{status: "unlocked", items: map[string]map[string]any{}, trash: map[string]map[string]any{}, folders: map[string]string{}}
}

func (f *fakeBW) id(prefix string) string {
	f.next++
	return fmt.Sprintf("%s-%04d", prefix, f.next)
}

func (f *fakeBW) fail(msg string) ([]byte, []byte, error) {
	return nil, []byte(msg), &RunError{Command: "bw", ExitCode: 1, Stderr: SanitizeStderr(msg)}
}

func (f *fakeBW) Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{name: name, args: append([]string(nil), args...), stdin: append([]byte(nil), stdin...)})
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if f.notInstalled {
		return nil, nil, &RunError{Command: "bw", ExitCode: -1, Err: &exec.Error{Name: name, Err: exec.ErrNotFound}}
	}
	var pos []string
	flags := map[string]string{}
	interactive := true
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--nointeraction":
			interactive = false
		case strings.HasPrefix(a, "--"):
			if i+1 < len(args) {
				flags[a] = args[i+1]
				i++
			}
		default:
			pos = append(pos, a)
		}
	}
	if interactive {
		return f.fail("fake bw: --nointeraction missing")
	}
	cmd := strings.Join(pos, " ")
	if cmd == "status" {
		out, _ := json.Marshal(map[string]any{"serverUrl": nil, "lastSync": "2026-01-01T00:00:00.000Z", "userEmail": "user@example.com", "status": f.status})
		return out, nil, nil
	}
	switch f.status {
	case "unauthenticated":
		return f.fail("You are not logged in.")
	case "locked":
		return f.fail("Vault is locked.")
	}
	if f.lockAfter > 0 && len(f.calls) > f.lockAfter {
		return f.fail("Vault is locked.")
	}

	switch {
	case cmd == "sync":
		return []byte("Syncing complete."), nil, nil
	case cmd == "list folders":
		out := []map[string]any{}
		search := strings.ToLower(flags["--search"])
		for id, n := range f.folders {
			if strings.Contains(strings.ToLower(n), search) {
				out = append(out, map[string]any{"object": "folder", "id": id, "name": n})
			}
		}
		if strings.Contains("no folder", search) {
			out = append(out, map[string]any{"object": "folder", "id": nil, "name": "No Folder"})
		}
		b, _ := json.Marshal(out)
		return b, nil, nil
	case cmd == "create folder":
		var req map[string]any
		if err := decodeRequest(stdin, &req); err != nil {
			return f.fail(err.Error())
		}
		id := f.id("folder")
		f.folders[id] = req["name"].(string)
		b, _ := json.Marshal(map[string]any{"object": "folder", "id": id, "name": req["name"]})
		return b, nil, nil
	case cmd == "list items":
		search := strings.ToLower(flags["--search"])
		ids := make([]string, 0, len(f.items))
		for id := range f.items {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		out := []map[string]any{}
		for _, id := range ids {
			it := f.items[id]
			if n, _ := it["name"].(string); strings.Contains(strings.ToLower(n), search) {
				out = append(out, it)
			}
		}
		b, _ := json.Marshal(out)
		return b, nil, nil
	case cmd == "create item":
		var req map[string]any
		if err := decodeRequest(stdin, &req); err != nil {
			return f.fail(err.Error())
		}
		id := f.id("item")
		req["id"] = id
		req["object"] = "item"
		req["revisionDate"] = "2026-01-01T00:00:00.000Z"
		f.items[id] = req
		b, _ := json.Marshal(req)
		return b, nil, nil
	case len(pos) == 3 && pos[0] == "edit" && pos[1] == "item":
		if _, ok := f.items[pos[2]]; !ok {
			return f.fail("Not found.")
		}
		var req map[string]any
		if err := decodeRequest(stdin, &req); err != nil {
			return f.fail(err.Error())
		}
		req["id"] = pos[2]
		f.items[pos[2]] = req
		b, _ := json.Marshal(req)
		return b, nil, nil
	case len(pos) == 3 && pos[0] == "delete" && pos[1] == "item":
		it, ok := f.items[pos[2]]
		if !ok {
			return f.fail("Not found.")
		}
		f.trash[pos[2]] = it
		delete(f.items, pos[2])
		return nil, nil, nil
	}
	return f.fail("fake bw: unsupported command " + cmd)
}

// decodeRequest mirrors bw: base64-decode stdin, then parse JSON.
func decodeRequest(stdin []byte, v any) error {
	if len(stdin) == 0 {
		return fmt.Errorf("`requestJson` was not provided.")
	}
	raw, err := base64.StdEncoding.DecodeString(string(stdin))
	if err != nil {
		return fmt.Errorf("Error parsing the encoded request data.")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("Error parsing the encoded request data.")
	}
	return nil
}

func (f *fakeBW) recorded() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

func (f *fakeBW) addItem(item map[string]any) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.id("item")
	item["id"] = id
	f.items[id] = item
	return id
}

// fakeBWS emulates the bws secret and project commands.
type fakeBWS struct {
	mu           sync.Mutex
	notInstalled bool
	reject       bool // respond like bws does for an invalid token
	secrets      map[string]map[string]any
	projects     map[string]bool
	next         int
	calls        []call
}

func newFakeBWS(projects ...string) *fakeBWS {
	f := &fakeBWS{secrets: map[string]map[string]any{}, projects: map[string]bool{}}
	for _, p := range projects {
		f.projects[p] = true
	}
	return f
}

func (f *fakeBWS) fail(msg string) ([]byte, []byte, error) {
	return nil, []byte(msg), &RunError{Command: "bws", ExitCode: 1, Stderr: SanitizeStderr(msg)}
}

func (f *fakeBWS) Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{name: name, args: append([]string(nil), args...), stdin: stdin})
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if f.notInstalled {
		return nil, nil, &RunError{Command: "bws", ExitCode: -1, Err: &exec.Error{Name: name, Err: exec.ErrNotFound}}
	}
	if f.reject {
		return f.fail("Error: \n   0: Received error message from server: [401 Unauthorized] {\"error\":\"invalid_client\"}")
	}
	var pos []string
	flags := map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			pos = append(pos, args[i+1:]...)
			i = len(args)
		case strings.HasPrefix(a, "--") && strings.Contains(a, "="):
			k, v, _ := strings.Cut(a, "=")
			flags[k] = v
		case strings.HasPrefix(a, "--"):
			if i+1 >= len(args) {
				return f.fail("error: a value is required for '" + a + "'")
			}
			flags[a] = args[i+1]
			i++
		case strings.HasPrefix(a, "-"):
			return f.fail("error: unexpected argument found")
		default:
			pos = append(pos, a)
		}
	}
	out := func(v any) ([]byte, []byte, error) {
		if flags["--output"] == "none" {
			return nil, nil, nil
		}
		b, _ := json.Marshal(v)
		return b, nil, nil
	}
	if len(pos) < 2 {
		return f.fail("error: unrecognized subcommand")
	}
	switch pos[0] + " " + pos[1] {
	case "project list":
		return out([]any{})
	case "project get":
		if len(pos) != 3 || !f.projects[pos[2]] {
			return f.fail("Error: 404 Not Found")
		}
		return out(map[string]any{"object": "project", "id": pos[2]})
	case "secret list":
		ids := make([]string, 0, len(f.secrets))
		for id := range f.secrets {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		list := []map[string]any{}
		for _, id := range ids {
			s := f.secrets[id]
			if len(pos) == 3 && s["projectId"] != pos[2] {
				continue
			}
			list = append(list, s)
		}
		return out(list)
	case "secret create":
		if len(pos) != 5 {
			return f.fail("error: wrong number of arguments")
		}
		if !f.projects[pos[4]] {
			return f.fail("Error: 404 Not Found")
		}
		f.next++
		id := fmt.Sprintf("secret-%04d", f.next)
		s := map[string]any{"object": "secret", "id": id, "key": pos[2], "value": pos[3], "note": flags["--note"], "projectId": pos[4]}
		f.secrets[id] = s
		return out(s)
	case "secret edit":
		if len(pos) != 3 {
			return f.fail("error: wrong number of arguments")
		}
		s, ok := f.secrets[pos[2]]
		if !ok {
			return f.fail("Error: 404 Not Found")
		}
		if v, ok := flags["--value"]; ok {
			s["value"] = v
		}
		return out(s)
	case "secret delete":
		if len(pos) != 3 {
			return f.fail("error: wrong number of arguments")
		}
		if _, ok := f.secrets[pos[2]]; !ok {
			return f.fail("Error: 404 Not Found")
		}
		delete(f.secrets, pos[2])
		return out(map[string]any{"successes": []string{pos[2]}})
	}
	return f.fail("error: unrecognized subcommand")
}

func (f *fakeBWS) recorded() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}
