package lsp

import "testing"

// VS Code sends a Windows path as file:///c%3A/Users/…. The escaped drive colon and the slash before the drive
// letter must not survive into the path.
func TestPathOfHandlesDriveLetters(t *testing.T) {
	for _, tc := range []struct {
		uri, goos, want string
		ok              bool
	}{
		{"file:///c%3A/Users/a%20b/x.go", "windows", `c:\Users\a b\x.go`, true},
		{"file:///C:/Users/x.go", "windows", `C:\Users\x.go`, true},
		{"file://server/share/pkg/demo.go", "windows", `\\server\share\pkg\demo.go`, true}, // a network share
		{"file:///Users/a%20b/x.go", "darwin", "/Users/a b/x.go", true},
		{"file:///home/a%20b/%C3%A9.go", "linux", "/home/a b/é.go", true},
		{"untitled:Untitled-1", "windows", "", false},
		{"file://", "linux", "", false},
	} {
		got, ok := pathOfFor(tc.uri, tc.goos)
		if got != tc.want || ok != tc.ok {
			t.Errorf("pathOfFor(%q, %s) = %q, %v; want %q, %v", tc.uri, tc.goos, got, ok, tc.want, tc.ok)
		}
	}
}

// Windows paths are case-insensitive, and editors disagree on the case of the drive letter. So a watched-file change
// must find its open siblings whatever the case.
func TestWatchedFilesMatchDirectoriesCaseInsensitivelyOnWindows(t *testing.T) {
	for _, tc := range []struct {
		a, b, goos string
		want       bool
	}{
		{`c:\Users\A\pkg`, `C:\users\a\PKG`, "windows", true},
		{`c:\Users\A\pkg`, `c:\Users\A\other`, "windows", false},
		{"/Users/A/pkg", "/Users/A/pkg", "darwin", true},
		{"/Users/A/pkg", "/users/a/pkg", "linux", false},
	} {
		if got := sameDir(tc.a, tc.b, tc.goos); got != tc.want {
			t.Errorf("sameDir(%q, %q, %s) = %v, want %v", tc.a, tc.b, tc.goos, got, tc.want)
		}
	}
}
