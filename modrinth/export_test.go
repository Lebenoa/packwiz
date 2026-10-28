package modrinth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/packwiz/packwiz/core"
)

func urlMod(rawURL string) *core.Mod {
	return &core.Mod{
		Name:     "Sodium",
		FileName: "mods/sodium.jar",
		Download: core.ModDownload{Mode: core.ModeURL, URL: rawURL},
	}
}

func cfMod(projectID, fileID int64) *core.Mod {
	return &core.Mod{
		Name:     "JourneyMap",
		FileName: "mods/journeymap.jar",
		Download: core.ModDownload{Mode: core.ModeCF},
		Update: map[string]map[string]interface{}{
			"curseforge": {"project-id": projectID, "file-id": fileID},
		},
	}
}

func TestExportURL(t *testing.T) {
	cfURLs := map[uint32]string{9099275: "https://edge.forgecdn.net/files/9099/275/instapack.jar"}

	tests := []struct {
		name       string
		mod        *core.Mod
		urls       map[uint32]string
		want       string
		wantErrSub string
	}{
		{
			name: "url mod keeps its own URL",
			mod:  urlMod("https://cdn.modrinth.com/data/AANobbMI/versions/abc/sodium.jar"),
			want: "https://cdn.modrinth.com/data/AANobbMI/versions/abc/sodium.jar",
		},
		{
			name: "non-whitelisted https host is still usable",
			mod:  urlMod("https://maven.example.com/sodium.jar"),
			want: "https://maven.example.com/sodium.jar",
		},
		{
			name:       "http is rejected",
			mod:        urlMod("http://example.com/sodium.jar"),
			wantErrSub: "HTTPS",
		},
		{
			name:       "empty url is rejected",
			mod:        urlMod("  "),
			wantErrSub: "no download URL",
		},
		{
			name: "curseforge resolves from the API map",
			mod:  cfMod(1733601, 9099275),
			urls: cfURLs,
			want: "https://edge.forgecdn.net/files/9099/275/instapack.jar",
		},
		{
			name:       "curseforge file without a URL is rejected",
			mod:        cfMod(1733601, 42),
			urls:       cfURLs,
			wantErrSub: "did not return a download URL",
		},
		{
			name:       "curseforge without a file ID is rejected",
			mod:        &core.Mod{Name: "Broken", FileName: "mods/broken.jar", Download: core.ModDownload{Mode: core.ModeCF}},
			urls:       cfURLs,
			wantErrSub: "invalid CurseForge file ID",
		},
		{
			name:       "unknown download mode is rejected",
			mod:        &core.Mod{Name: "Future", FileName: "mods/future.jar", Download: core.ModDownload{Mode: "metadata:elsewhere"}},
			wantErrSub: "unsupported download mode",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := exportURL(tt.mod, tt.urls)
			if tt.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrSub) {
					t.Fatalf("expected error containing %q, got (%q, %v)", tt.wantErrSub, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCheckURLExportable(t *testing.T) {
	good := []*core.Mod{urlMod("https://example.com/a.jar"), cfMod(1, 2)}
	if err := checkURLExportable(good, map[uint32]string{2: "https://edge.forgecdn.net/files/x.jar"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	bad := []*core.Mod{urlMod("https://example.com/a.jar"), cfMod(1, 999)}
	err := checkURLExportable(bad, map[uint32]string{})
	if err == nil {
		t.Fatal("expected a missing CurseForge URL to be reported")
	}
	if !strings.Contains(err.Error(), "JourneyMap") {
		t.Fatalf("error should name the file: %v", err)
	}
}

func TestManifestEnv(t *testing.T) {
	tests := []struct {
		name       string
		mod        *core.Mod
		wantClient string
		wantServer string
	}{
		{name: "universal", mod: &core.Mod{Side: core.UniversalSide}, wantClient: "required", wantServer: "required"},
		{name: "empty side is universal", mod: &core.Mod{Side: core.EmptySide}, wantClient: "required", wantServer: "required"},
		{name: "client", mod: &core.Mod{Side: core.ClientSide}, wantClient: "required", wantServer: "unsupported"},
		{name: "server", mod: &core.Mod{Side: core.ServerSide}, wantClient: "unsupported", wantServer: "required"},
		{
			name:       "optional",
			mod:        &core.Mod{Side: core.UniversalSide, Option: &core.ModOption{Optional: true}},
			wantClient: "optional",
			wantServer: "optional",
		},
		{
			name:       "optional and client only",
			mod:        &core.Mod{Side: core.ClientSide, Option: &core.ModOption{Optional: true}},
			wantClient: "optional",
			wantServer: "unsupported",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, server := manifestEnv(tt.mod)
			if client != tt.wantClient || server != tt.wantServer {
				t.Fatalf("got (%q, %q), want (%q, %q)", client, server, tt.wantClient, tt.wantServer)
			}
		})
	}
}

func completed(mod *core.Mod, hashes map[string]string) core.CompletedDownload {
	return core.CompletedDownload{Mod: mod, Hashes: hashes}
}

func TestBuildManifestFile(t *testing.T) {
	t.Run("full entry", func(t *testing.T) {
		mod := urlMod("https://cdn.modrinth.com/data/AANobbMI/versions/abc/sodium.jar")
		file, err := buildManifestFile(completed(mod, map[string]string{
			"sha1":         "003c114c8e2b1e1e4a8d6b4a8dc1e0b4b1b2c3d4",
			"sha512":       "aa11bb22",
			"length-bytes": "41920",
		}), "mods/sodium.jar", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if file.Path != "mods/sodium.jar" {
			t.Fatalf("path = %q", file.Path)
		}
		if file.Hashes["sha1"] != "003c114c8e2b1e1e4a8d6b4a8dc1e0b4b1b2c3d4" || file.Hashes["sha512"] != "aa11bb22" {
			t.Fatalf("hashes = %v", file.Hashes)
		}
		if file.FileSize != 41920 {
			t.Fatalf("fileSize = %d", file.FileSize)
		}
		if len(file.Downloads) != 1 || file.Downloads[0] != mod.Download.URL {
			t.Fatalf("downloads = %v", file.Downloads)
		}
		if file.Env == nil || file.Env.Client != "required" || file.Env.Server != "required" {
			t.Fatalf("env = %+v", file.Env)
		}
	})

	t.Run("file size must fit 32 bits", func(t *testing.T) {
		_, err := buildManifestFile(completed(urlMod("https://example.com/a.jar"), map[string]string{
			"sha1": "a", "sha512": "b", "length-bytes": "4294967296",
		}), "mods/a.jar", nil)
		if err == nil || !strings.Contains(err.Error(), "32-bit") {
			t.Fatalf("expected a fileSize overflow error, got %v", err)
		}
	})

	t.Run("missing hashes without a file", func(t *testing.T) {
		_, err := buildManifestFile(completed(urlMod("https://example.com/a.jar"), map[string]string{"sha1": "a"}), "mods/a.jar", nil)
		if err == nil || !strings.Contains(err.Error(), "SHA-512") {
			t.Fatalf("expected a missing hash error, got %v", err)
		}
	})

	t.Run("hashes are computed when the session did not supply them", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "cached.jar")
		if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
			t.Fatal(err)
		}
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()

		dl := completed(urlMod("https://example.com/a.jar"), map[string]string{"sha256": "unused"})
		dl.File = file
		entry, err := buildManifestFile(dl, "mods/a.jar", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if entry.Hashes["sha1"] != "aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d" {
			t.Fatalf("sha1 = %q", entry.Hashes["sha1"])
		}
		if entry.FileSize != 5 {
			t.Fatalf("fileSize = %d", entry.FileSize)
		}
	})
}

// TestPackFileJSON locks the on-disk field names launchers read.
func TestPackFileJSON(t *testing.T) {
	entry := PackFile{
		Path:      "mods/sodium.jar",
		Hashes:    map[string]string{"sha1": "a", "sha512": "b"},
		Env:       &PackEnv{Client: "required", Server: "unsupported"},
		Downloads: []string{"https://example.com/sodium.jar"},
		FileSize:  1234,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"path", "hashes", "env", "downloads", "fileSize"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("manifest entry is missing %q: %s", key, data)
		}
	}
	env, ok := decoded["env"].(map[string]interface{})
	if !ok {
		t.Fatalf("env is not an object: %s", data)
	}
	if env["client"] != "required" || env["server"] != "unsupported" {
		t.Fatalf("env = %v", env)
	}
}
