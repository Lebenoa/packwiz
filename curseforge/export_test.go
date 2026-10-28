package curseforge

import (
	"strings"
	"testing"

	"github.com/packwiz/packwiz/core"
)

func TestGetFileID(t *testing.T) {
	tests := []struct {
		name       string
		update     map[string]map[string]interface{}
		want       uint32
		wantErrSub string
	}{
		{
			name:   "int64 from TOML",
			update: map[string]map[string]interface{}{"curseforge": {"file-id": int64(9099275)}},
			want:   9099275,
		},
		{
			name:   "string value",
			update: map[string]map[string]interface{}{"curseforge": {"file-id": "42"}},
			want:   42,
		},
		{
			name:       "project id alone is not enough",
			update:     map[string]map[string]interface{}{"curseforge": {"project-id": int64(1)}},
			wantErrSub: "invalid CurseForge file ID",
		},
		{
			name:       "non-numeric value",
			update:     map[string]map[string]interface{}{"curseforge": {"file-id": "abc"}},
			wantErrSub: "invalid CurseForge file ID",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mod := &core.Mod{Name: "InstaPack", FileName: "mods/instapack.jar", Update: tt.update}
			got, err := GetFileID(mod)
			if tt.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("expected error containing %q, got (%d, %v)", tt.wantErrSub, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestFileInfoFromAPI(t *testing.T) {
	infos := fileInfoFromAPI([]modFileInfo{
		{ID: 9099256, FileName: "instapack-fabric-26.3-1.0.0.jar", DownloadURL: "https://edge.forgecdn.net/files/9099/256/instapack-fabric-26.3-1.0.0.jar", Fingerprint: 273936777},
		{ID: 42, FileName: "no-url.jar"},
	})

	file, ok := infos[9099256]
	if !ok {
		t.Fatal("file 9099256 missing from the resolved metadata")
	}
	if file.DownloadURL != "https://edge.forgecdn.net/files/9099/256/instapack-fabric-26.3-1.0.0.jar" {
		t.Fatalf("download url = %q", file.DownloadURL)
	}
	if file.Fingerprint == nil || *file.Fingerprint != 273936777 {
		t.Fatalf("fingerprint = %v, want 273936777", file.Fingerprint)
	}

	// A response without fileFingerprint must not be turned into a fingerprint of zero.
	file, ok = infos[42]
	if !ok {
		t.Fatal("file 42 missing from the resolved metadata")
	}
	if file.Fingerprint != nil {
		t.Fatalf("missing fingerprint should stay unset, got %d", *file.Fingerprint)
	}
	if file.DownloadURL != "" {
		t.Fatalf("download url = %q, want empty", file.DownloadURL)
	}
}

func TestResolveModDownloadURLsIgnoresOtherSources(t *testing.T) {
	// No CurseForge mods means no API call at all.
	mods := []*core.Mod{
		{Name: "Sodium", FileName: "mods/sodium.jar", Download: core.ModDownload{Mode: core.ModeURL, URL: "https://example.com/a.jar"}},
	}
	urls, err := ResolveModDownloadURLs(mods)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(urls) != 0 {
		t.Fatalf("expected no URLs, got %v", urls)
	}

	files, err := ResolveModFiles(mods)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("expected no files, got %v", files)
	}
}
