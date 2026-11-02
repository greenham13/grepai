package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPathPolicyWalkToleratesDisappearingDescendants(t *testing.T) {
	for _, mode := range []string{"discovery", "metadata", "reconcile", "full"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			putPolicyFile(t, root, "a.go", "package a")
			putPolicyFile(t, root, "z-dir/file.go", "package z")
			armed := mode == "discovery"
			policyFixture(t, &testPolicy{exclude: func(path string) (bool, error) {
				if armed && path == "a.go" {
					armed = false
					if err := os.RemoveAll(filepath.Join(root, "z-dir")); err != nil {
						t.Fatal(err)
					}
				}
				return false, nil
			}})
			matcher, err := NewIgnoreMatcher(root, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			if mode == "discovery" {
				return
			}
			armed = true
			scanner := NewScanner(root, matcher)
			switch mode {
			case "metadata":
				_, _, err = scanner.ScanMetadata()
			case "reconcile":
				_, _, err = scanner.ScanMetadataContext(context.Background())
			case "full":
				_, _, err = scanner.Scan()
			}
			if err != nil {
				t.Fatalf("vanished descendant failed %s walk: %v", mode, err)
			}
		})
	}
}

func TestPathPolicyMissingRootRemainsFatal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	policyFixture(t, &testPolicy{exclude: func(string) (bool, error) { return false, nil }})
	matcher, err := NewIgnoreMatcher(root, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	scanner := NewScanner(root, matcher)
	for _, mode := range []string{"discovery", "metadata", "reconcile", "full"} {
		t.Run(mode, func(t *testing.T) {
			switch mode {
			case "discovery":
				_, err = NewIgnoreMatcher(root, nil, "")
			case "metadata":
				_, _, err = scanner.ScanMetadata()
			case "reconcile":
				_, _, err = scanner.ScanMetadataContext(context.Background())
			case "full":
				_, _, err = scanner.Scan()
			}
			if !IsPathPolicyError(err) || !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("missing root failure suppressed: %v", err)
			}
		})
	}
}

func TestPathPolicyReturnedENOENTRemainsFatal(t *testing.T) {
	root := t.TempDir()
	putPolicyFile(t, root, "a.go", "package a")
	armed := false
	policyFixture(t, &testPolicy{exclude: func(path string) (bool, error) {
		if armed && path == "a.go" {
			return false, os.ErrNotExist
		}
		return false, nil
	}})
	matcher, err := NewIgnoreMatcher(root, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	armed = true
	scanner := NewScanner(root, matcher)
	for _, mode := range []string{"discovery", "metadata", "reconcile", "full"} {
		t.Run(mode, func(t *testing.T) {
			switch mode {
			case "discovery":
				_, err = NewIgnoreMatcher(root, nil, "")
			case "metadata":
				_, _, err = scanner.ScanMetadata()
			case "reconcile":
				_, _, err = scanner.ScanMetadataContext(context.Background())
			case "full":
				_, _, err = scanner.Scan()
			}
			if !IsPathPolicyError(err) || !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("actual policy failure suppressed: %v", err)
			}
		})
	}
}
