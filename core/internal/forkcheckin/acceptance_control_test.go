package forkcheckin_test

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// The historical executable is already built and is never replaced. This
// second control contains exactly the reviewed configuration-write retries
// (service update/delete, policy update, access-token create) and their helper,
// not the extension, HTTP registration, or any main changes.
func prepareAcceptanceStorageControl(t *testing.T, baseline, current string) {
	t.Helper()
	for _, item := range []struct {
		file  string
		names []string
	}{
		{"service_store.go", []string{"UpdateService", "DeleteService"}},
		{"policy_store.go", []string{"UpdatePolicy"}},
		{"store.go", []string{"CreateAccessToken", "insertAccessTokenTx"}},
	} {
		relative := filepath.Join("internal", "storage", "sqlite", item.file)
		old, err := os.ReadFile(filepath.Join(baseline, relative))
		if err != nil {
			t.Fatal(err)
		}
		next, err := os.ReadFile(filepath.Join(current, relative))
		if err != nil {
			t.Fatal(err)
		}
		patched, err := acceptancePatchFunctions(old, next, item.names)
		if err != nil {
			t.Fatalf("storage-only control %s: %v", item.file, err)
		}
		if err := os.WriteFile(filepath.Join(baseline, relative), patched, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("storage-only control file=%s sha256=%x", relative, sha256.Sum256(patched))
	}
	relative := filepath.Join("internal", "storage", "sqlite", "config_write.go")
	helper, err := os.ReadFile(filepath.Join(current, relative))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baseline, relative), helper, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("storage-only control file=%s sha256=%x; original historical baseline retained separately", relative, sha256.Sum256(helper))
}

func acceptancePatchFunctions(old, next []byte, names []string) ([]byte, error) {
	type span struct{ start, end int }
	find := func(source []byte) (map[string]span, error) {
		positions := token.NewFileSet()
		file, err := parser.ParseFile(positions, "fixture.go", source, 0)
		if err != nil {
			return nil, err
		}
		found := make(map[string]span)
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if _, duplicate := found[function.Name.Name]; duplicate {
				return nil, fmt.Errorf("ambiguous function %s", function.Name.Name)
			}
			found[function.Name.Name] = span{positions.Position(function.Pos()).Offset, positions.Position(function.End()).Offset}
		}
		return found, nil
	}
	before, err := find(old)
	if err != nil {
		return nil, err
	}
	after, err := find(next)
	if err != nil {
		return nil, err
	}
	ordered := append([]string(nil), names...)
	for _, name := range ordered {
		if _, ok := before[name]; !ok {
			return nil, fmt.Errorf("missing old function %s", name)
		}
		if _, ok := after[name]; !ok {
			return nil, fmt.Errorf("missing new function %s", name)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return before[ordered[i]].start > before[ordered[j]].start })
	patched := append([]byte(nil), old...)
	for _, name := range ordered {
		from, to := before[name], after[name]
		patched = bytes.Join([][]byte{patched[:from.start], next[to.start:to.end], patched[from.end:]}, nil)
	}
	if !bytes.Equal(patched, next) {
		return nil, fmt.Errorf("unreviewed changes outside the configuration-write functions")
	}
	return patched, nil
}

func TestAcceptanceStorageControlRejectsUnrelatedChanges(t *testing.T) {
	old := []byte("package fixture\n\nfunc UpdateService() { old() }\nfunc Other() {}\n")
	next := bytes.ReplaceAll(old, []byte("old()"), []byte("fixed()"))
	if patched, err := acceptancePatchFunctions(old, next, []string{"UpdateService"}); err != nil || !bytes.Equal(patched, next) {
		t.Fatalf("reviewed change rejected: %v", err)
	}
	for _, source := range [][]byte{
		bytes.ReplaceAll(next, []byte("func Other() {}"), []byte("func Other() { changed() }")),
		append(append([]byte(nil), next...), []byte("func Extension() {}\n")...),
		bytes.ReplaceAll(next, []byte("UpdateService"), []byte("Renamed")),
	} {
		if _, err := acceptancePatchFunctions(old, source, []string{"UpdateService"}); err == nil {
			t.Fatal("storage control accepted an unrelated or missing function")
		}
	}
}
