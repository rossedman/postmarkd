package config

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode"
)

// FuzzParseFile checks the KEY=VALUE parser on arbitrary file contents: it
// never panics, is deterministic, produces trimmed keys and values without
// line breaks or '=' in keys, and a file rendered back from the parsed map
// parses to the same map.
func FuzzParseFile(f *testing.F) {
	for _, seed := range []string{
		"",
		"A=1\n",
		"# comment\n; comment\n\nexport A=\"quoted value\"\nB='single'\n",
		"A=b=c\r\nB = spaced \n",
		"export export A=1\n",
		"export #A=1\n",
		"=empty key\n",
		"A=\"\n",
		"A=\"x'\n",
		"no equals\n",
		"A=\"  inner  \"\nA=last\n",
		"A B=1\n\tC=\v2\v\n",
	} {
		f.Add(seed)
	}
	// Iterations within a worker run sequentially, so they share a directory.
	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, content string) {
		if len(content) > 32<<10 {
			return // stay well under bufio.MaxScanTokenSize after rendering
		}
		path := filepath.Join(dir, "env")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := ParseFile(path)
		again, err2 := ParseFile(path)
		if (err == nil) != (err2 == nil) || !reflect.DeepEqual(got, again) {
			t.Fatalf("not deterministic: %q/%v vs %q/%v", got, err, again, err2)
		}
		if err != nil {
			if got != nil {
				t.Fatalf("map %q returned with error %v", got, err)
			}
			return
		}
		for k, v := range got {
			if strings.ContainsAny(k, "=\n") {
				t.Errorf("key %q contains '=' or a newline", k)
			}
			if k != strings.TrimSpace(k) {
				t.Errorf("key %q is not trimmed", k)
			}
			if strings.Contains(v, "\n") {
				t.Errorf("value %q for %q contains a newline", v, k)
			}
		}

		// Render back. A leading "export " protects keys that would
		// otherwise read as comments ("#x") or as an export prefix
		// ("export x"); values are always double quoted, which the parser
		// strips without interpreting escapes.
		var b strings.Builder
		for _, k := range slices.Sorted(maps.Keys(got)) {
			b.WriteString("export " + k + `="` + got[k] + "\"\n")
		}
		rendered := filepath.Join(dir, "rendered")
		if err := os.WriteFile(rendered, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		back, err := ParseFile(rendered)
		if err != nil {
			t.Fatalf("rendered file %q does not parse: %v", b.String(), err)
		}
		if !reflect.DeepEqual(back, got) {
			t.Fatalf("round trip mismatch:\n got  %q\n back %q\n file %q", got, back, b.String())
		}
	})
}

// FuzzParseAddrs checks the listen address list parser: every entry is a
// trimmed, non-empty tcp address or a unix socket, and rendering the result
// back parses to the same list.
func FuzzParseAddrs(f *testing.F) {
	for _, seed := range []string{
		"", ",", "127.0.0.1:8025", " unix:/run/a.sock , /run/b.sock,:25,", "unix:", "unix: x", "[::1]:25,unix:unix:x",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, list string) {
		addrs := ParseAddrs(list)
		if len(addrs) > strings.Count(list, ",")+1 {
			t.Fatalf("%d addresses from %d fields", len(addrs), strings.Count(list, ",")+1)
		}
		rendered := make([]string, 0, len(addrs))
		for _, a := range addrs {
			if strings.Contains(a.Address, ",") {
				t.Errorf("address %q contains a comma", a.Address)
			}
			switch a.Network {
			case "tcp":
				if a.Address == "" || a.Address != strings.TrimSpace(a.Address) {
					t.Errorf("tcp address %q is empty or untrimmed", a.Address)
				}
				if strings.HasPrefix(a.Address, "/") || strings.HasPrefix(a.Address, "unix:") {
					t.Errorf("tcp address %q looks like a unix socket", a.Address)
				}
				rendered = append(rendered, a.Address)
			case "unix":
				if strings.TrimRightFunc(a.Address, unicode.IsSpace) != a.Address {
					t.Errorf("unix address %q has trailing space", a.Address)
				}
				rendered = append(rendered, "unix:"+a.Address)
			default:
				t.Fatalf("unknown network %q", a.Network)
			}
		}
		if back := ParseAddrs(strings.Join(rendered, ",")); !reflect.DeepEqual(back, addrs) {
			t.Fatalf("round trip: %v -> %v", addrs, back)
		}
	})
}

// FuzzSplitTokens checks the auth token list splitter: tokens are never
// empty and never contain separators or comments.
func FuzzSplitTokens(f *testing.F) {
	for _, seed := range []string{"", "a,b c", "# c\nweb # app\r\nworker\t,x", ",,\n#\n"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, tok := range splitTokens(s) {
			if tok == "" || strings.ContainsAny(tok, ", \t\r\n#") {
				t.Fatalf("bad token %q from %q", tok, s)
			}
			if !strings.Contains(s, tok) {
				t.Fatalf("token %q is not a substring of %q", tok, s)
			}
		}
	})
}

// FuzzLoadEnv feeds arbitrary values for every known key through Load: it
// never panics, is deterministic, and a Config it accepts always validates.
func FuzzLoadEnv(f *testing.F) {
	keys := slices.Sorted(maps.Keys(known))
	f.Add(uint8(0), "x")
	f.Add(uint8(3), "0660")
	f.Add(uint8(10), "1m30s")
	f.Add(uint8(20), "-1")
	f.Fuzz(func(t *testing.T, i uint8, value string) {
		key := keys[int(i)%len(keys)]
		if strings.HasSuffix(key, "_FILE") {
			return // would read arbitrary paths from the host
		}
		env := []string{Prefix + "BLACKHOLE=true", key + "=" + value}
		cfg, err := Load("", env)
		cfg2, err2 := Load("", env)
		if (err == nil) != (err2 == nil) || !reflect.DeepEqual(cfg, cfg2) {
			t.Fatalf("not deterministic for %s=%q", key, value)
		}
		if err == nil {
			if verr := cfg.Validate(); verr != nil {
				t.Fatalf("Load accepted %s=%q but Validate fails: %v", key, value, verr)
			}
		}
	})
}
