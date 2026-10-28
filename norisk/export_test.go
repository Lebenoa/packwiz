package norisk

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/packwiz/packwiz/cmdshared"
	"github.com/packwiz/packwiz/core"
)

func TestLoaderFor(t *testing.T) {
	tests := []struct {
		name          string
		versions      map[string]string
		wantLoader    string
		wantVersion   string
		wantErrSubstr string
	}{
		{
			name:        "fabric",
			versions:    map[string]string{"minecraft": "26.2", "fabric": "0.20.1"},
			wantLoader:  "fabric",
			wantVersion: "0.20.1",
		},
		{
			name:        "quilt wins over fabric",
			versions:    map[string]string{"minecraft": "1.21", "quilt": "0.28.0", "fabric": "0.16.0"},
			wantLoader:  "quilt",
			wantVersion: "0.28.0",
		},
		{
			name:        "neoforge",
			versions:    map[string]string{"minecraft": "1.21.1", "neoforge": "21.1.80"},
			wantLoader:  "neoforge",
			wantVersion: "21.1.80",
		},
		{
			name:        "forge",
			versions:    map[string]string{"minecraft": "1.20.1", "forge": "47.2.0"},
			wantLoader:  "forge",
			wantVersion: "47.2.0",
		},
		{
			name:        "vanilla",
			versions:    map[string]string{"minecraft": "1.21"},
			wantLoader:  "vanilla",
			wantVersion: "",
		},
		{
			name:          "unsupported loader",
			versions:      map[string]string{"minecraft": "1.21", "purpur": "1.21"},
			wantErrSubstr: "purpur",
		},
		{
			name:          "no minecraft version",
			versions:      map[string]string{"fabric": "0.20.1"},
			wantErrSubstr: "no minecraft version",
		},
		{
			name:          "empty minecraft version",
			versions:      map[string]string{"minecraft": "", "fabric": "0.20.1"},
			wantErrSubstr: "no minecraft version",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pack := core.Pack{Versions: tt.versions}
			loader, version, err := loaderFor(pack)
			if tt.wantErrSubstr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrSubstr) {
					t.Fatalf("expected error containing %q, got %v", tt.wantErrSubstr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if loader != tt.wantLoader || version != tt.wantVersion {
				t.Fatalf("got (%q, %q), want (%q, %q)", loader, version, tt.wantLoader, tt.wantVersion)
			}
		})
	}
}

func TestModFileName(t *testing.T) {
	mod := &core.Mod{Name: "Shader", FileName: "mods/shader.zip"}
	name, err := modFileName(mod)
	if err != nil || name != "shader.zip" {
		t.Fatalf("got (%q, %v)", name, err)
	}

	if _, err := modFileName(&core.Mod{Name: "Config", FileName: "config.yaml"}); err == nil {
		t.Fatal("expected non-jar file to be rejected")
	}
}

func TestIsModRecordPath(t *testing.T) {
	cases := map[string]bool{
		"mods/sodium.jar":   true,
		"mods/some-mod.zip": true, "mods/subdir/sodium.jar": true, // launcher flattens mods into one folder
		"root-mod.jar":                true, // legacy root-level metadata
		"resourcepacks/pack.zip":      false,
		"resourcepacks/root-pack.zip": false,
		"shaderpacks/shader.zip":      false,
		"config/some-bundled-mod.jar": false,
		"options.txt":                 false,
	}
	for p, want := range cases {
		if got := isModRecordPath(p); got != want {
			t.Errorf("isModRecordPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestCheckRepresentable(t *testing.T) {
	valid := &core.Mod{
		Name:     "Sodium",
		FileName: "mods/sodium.jar",
		Download: core.ModDownload{Mode: core.ModeURL, URL: "https://cdn.modrinth.com/data/x/versions/y/sodium.jar"},
	}
	if err := checkRepresentable([]*core.Mod{valid}); err != nil {
		t.Fatalf("valid mod rejected: %v", err)
	}

	for _, rawURL := range []string{"file:///tmp/sodium.jar", "http://example.com/sodium.jar", ""} {
		mod := &core.Mod{
			Name:     "Local",
			FileName: "mods/local.jar",
			Download: core.ModDownload{Mode: core.ModeURL, URL: rawURL},
		}
		err := checkRepresentable([]*core.Mod{mod})
		if err == nil || !strings.Contains(err.Error(), "HTTPS") {
			t.Errorf("URL %q: expected HTTPS error, got %v", rawURL, err)
		}
	}
}

func TestBuildModRecord(t *testing.T) {
	mc := "26.2"
	loader := "fabric"

	t.Run("modrinth", func(t *testing.T) {
		mod := &core.Mod{
			Name:     "Sodium",
			FileName: "mods/sodium.jar",
			Side:     core.UniversalSide,
			Download: core.ModDownload{
				Mode: core.ModeURL,
				URL:  "https://cdn.modrinth.com/data/AANobbMI/versions/abc/sodium.jar",
			},
			Update: map[string]map[string]interface{}{
				"modrinth": {"mod-id": "AANobbMI", "version": "abc"},
			},
		}
		record, err := buildModRecord(core.CompletedDownload{
			Mod:    mod,
			Hashes: map[string]string{"sha1": "deadbeef"},
		}, nil, mc, loader)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if record.Source.Type != sourceModrinth {
			t.Fatalf("source type = %q", record.Source.Type)
		}
		if record.Source.ProjectID != "AANobbMI" || record.Source.VersionID != "abc" {
			t.Fatalf("unexpected ids: %+v", record.Source)
		}
		if record.Source.FileHash != "deadbeef" {
			t.Fatalf("sha1 not carried over: %+v", record.Source)
		}
		if record.AssociatedLoader != loader {
			t.Fatalf("associated_loader = %q, want %q (launcher skips mods without it)", record.AssociatedLoader, loader)
		}
		if record.UpdatesEnabled {
			t.Fatal("pack-managed mods must not auto-update")
		}
		if len(record.GameVersions) != 1 || record.GameVersions[0] != mc {
			t.Fatalf("game_versions = %v", record.GameVersions)
		}
		if record.ModpackOrigin != "modrinth:AANobbMI:abc" {
			t.Fatalf("modpack_origin = %q", record.ModpackOrigin)
		}
	})

	t.Run("curseforge metadata", func(t *testing.T) {
		mod := &core.Mod{
			Name:     "Just Enough Items",
			FileName: "mods/jei.jar",
			Download: core.ModDownload{Mode: core.ModeCF},
			Update: map[string]map[string]interface{}{
				"curseforge": {"project-id": int64(238222), "file-id": int64(6829086)},
			},
		}
		urls := map[uint32]string{6829086: "https://edge.forgecdn.net/files/6829/86/jei.jar"}
		record, err := buildModRecord(core.CompletedDownload{
			Mod:    mod,
			Hashes: map[string]string{"sha1": "cafe"},
		}, urls, mc, loader)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if record.Source.Type != sourceCurseForge {
			t.Fatalf("source type = %q", record.Source.Type)
		}
		if record.Source.ProjectID != "238222" || record.Source.FileID != "6829086" {
			t.Fatalf("unexpected ids: %+v", record.Source)
		}
		if record.Source.DownloadURL != urls[6829086] {
			t.Fatalf("download url = %q", record.Source.DownloadURL)
		}
	})

	t.Run("curseforge without url", func(t *testing.T) {
		mod := &core.Mod{
			Name:     "InstaPack",
			FileName: "mods/instapack.jar",
			Download: core.ModDownload{Mode: core.ModeCF},
			Update:   map[string]map[string]interface{}{"curseforge": {"project-id": int64(1), "file-id": int64(2)}},
		}
		if _, err := buildModRecord(core.CompletedDownload{Mod: mod}, nil, mc, loader); err == nil {
			t.Fatal("expected an error when CurseForge returns no download URL")
		}
	})

	t.Run("plain https url", func(t *testing.T) {
		mod := &core.Mod{
			Name:     "Custom",
			FileName: "mods/custom.jar",
			Download: core.ModDownload{Mode: core.ModeURL, URL: "https://example.com/custom.jar"},
		}
		record, err := buildModRecord(core.CompletedDownload{
			Mod:    mod,
			Hashes: map[string]string{"sha1": "f00d"},
		}, nil, mc, loader)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if record.Source.Type != sourceURL || record.Source.URL != "https://example.com/custom.jar" {
			t.Fatalf("unexpected source: %+v", record.Source)
		}
		// The URL variant has no download_url or hash field; ensure it is not serialized.
		encoded, err := json.Marshal(record.Source)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "download_url") || strings.Contains(string(encoded), "file_hash_sha1") {
			t.Fatalf("url source leaked metadata fields: %s", encoded)
		}
	})

	t.Run("optional mod defaults disabled", func(t *testing.T) {
		mod := &core.Mod{
			Name:     "Optional",
			FileName: "mods/optional.jar",
			Download: core.ModDownload{Mode: core.ModeURL, URL: "https://example.com/optional.jar"},
			Option:   &core.ModOption{Optional: true},
		}
		record, err := buildModRecord(core.CompletedDownload{Mod: mod}, nil, mc, loader)
		if err != nil {
			t.Fatal(err)
		}
		if record.Enabled {
			t.Fatal("optional mod without default should be exported disabled")
		}

		mod.Option.Default = true
		record, err = buildModRecord(core.CompletedDownload{Mod: mod}, nil, mc, loader)
		if err != nil {
			t.Fatal(err)
		}
		if !record.Enabled {
			t.Fatal("optional mod opted in by default should be exported enabled")
		}
	})

	t.Run("rejects insecure url", func(t *testing.T) {
		mod := &core.Mod{
			Name:     "Local",
			FileName: "mods/local.jar",
			Download: core.ModDownload{Mode: core.ModeURL, URL: "http://example.com/local.jar"},
		}
		if _, err := buildModRecord(core.CompletedDownload{Mod: mod}, nil, mc, loader); err == nil {
			t.Fatal("expected insecure URL to be rejected")
		}
	})
}

func TestBuildBanner(t *testing.T) {
	if banner, err := buildBanner(""); err != nil || banner != nil {
		t.Fatalf("empty icon should produce no banner, got (%v, %v)", banner, err)
	}

	https, err := buildBanner("https://example.com/icon.png")
	if err != nil {
		t.Fatal(err)
	}
	if https.Source.Type != "url" || https.Source.URL != "https://example.com/icon.png" {
		t.Fatalf("unexpected banner: %+v", https.Source)
	}

	if _, err := buildBanner("http://example.com/icon.png"); err == nil {
		t.Fatal("http icon should be rejected (importer strips it)")
	}

	dataURI, err := buildBanner("data:image/gif;base64,R0lGODlhAQABAAAAACw=")
	if err != nil {
		t.Fatal(err)
	}
	if dataURI.Source.Type != "base64" || dataURI.Source.MimeType != "image/gif" || dataURI.Source.Data != "R0lGODlhAQABAAAAACw=" {
		t.Fatalf("unexpected data URI banner: %+v", dataURI.Source)
	}
	if _, err := buildBanner("data:text/plain;base64,aGk="); err == nil {
		t.Fatal("non-image data URI should be rejected")
	}
	if _, err := buildBanner("data:image/png;base64,"); err == nil {
		t.Fatal("empty data URI payload should be rejected")
	}

	dir := t.TempDir()
	iconPath := filepath.Join(dir, "icon.png")
	if err := os.WriteFile(iconPath, []byte("not-a-real-png"), 0o644); err != nil {
		t.Fatal(err)
	}
	local, err := buildBanner(iconPath)
	if err != nil {
		t.Fatal(err)
	}
	if local.Source.Type != "base64" || local.Source.MimeType != "image/png" {
		t.Fatalf("unexpected local banner: %+v", local.Source)
	}
	if _, err := buildBanner(filepath.Join(dir, "missing.png")); err == nil {
		t.Fatal("missing icon should be rejected")
	}

	textPath := filepath.Join(dir, "icon.txt")
	if err := os.WriteFile(textPath, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := buildBanner(textPath); err == nil {
		t.Fatal("unsupported image extension should be rejected")
	}
}

// TestWriteArchive checks the archive layout and the profile.json contract the NoRisk
// importer relies on, without needing network access.
func TestWriteArchive(t *testing.T) {
	packDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(packDir, "options.txt"), []byte("fov:0.5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(packDir, "resourcepacks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "resourcepacks", "pack.zip"), []byte("zip-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	indexToml := "hash-format = \"sha256\"\n\n[[files]]\nfile = \"options.txt\"\nhash = \"00\"\n\n[[files]]\nfile = \"resourcepacks/pack.zip\"\nhash = \"00\"\n"
	if err := os.WriteFile(filepath.Join(packDir, "index.toml"), []byte(indexToml), 0o644); err != nil {
		t.Fatal(err)
	}
	index, err := core.LoadIndex(filepath.Join(packDir, "index.toml"))
	if err != nil {
		t.Fatal(err)
	}

	profile := newProfile(core.Pack{
		Name:        "Leben's NVIDIA Pack",
		Description: "NVIDIA-flavoured Fabric pack",
		Versions:    map[string]string{"minecraft": "26.2", "fabric": "0.20.1"},
	}, "26.2", "fabric", "0.20.1", &ProfileBanner{
		Source: ImageSource{Type: "base64", Data: "AAAA", MimeType: "image/png"},
	})
	profile.Mods = append(profile.Mods, Mod{
		ID:               stableUUID("mod", "sodium.jar", "https://cdn.modrinth.com/x"),
		Source:           ModSource{Type: sourceModrinth, ProjectID: "AANobbMI", VersionID: "abc", FileName: "sodium.jar", DownloadURL: "https://cdn.modrinth.com/x", FileHash: "deadbeef"},
		Enabled:          true,
		DisplayName:      "Sodium",
		GameVersions:     []string{"26.2"},
		AssociatedLoader: "fabric",
		UpdatesEnabled:   false,
	})

	out := filepath.Join(t.TempDir(), "pack.noriskpack")
	if err := writeArchive(out, func(zw *zip.Writer) error {
		if _, err := zw.Create("overrides/"); err != nil {
			return err
		}
		// Exercise the same helper export uses for ordinary overrides.
		cmdshared.AddNonMetafileOverrides(&index, zw)
		return writeProfileJSON(zw, profile)
	}); err != nil {
		t.Fatalf("writeArchive: %v", err)
	}

	reader, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	entries := make(map[string][]byte)
	for _, f := range reader.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		_ = rc.Close()
		entries[f.Name] = data
	}
	for _, want := range []string{"profile.json", "overrides/", "overrides/options.txt", "overrides/resourcepacks/pack.zip"} {
		if _, ok := entries[want]; !ok {
			t.Fatalf("archive is missing %q (has %v)", want, keys(entries))
		}
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(entries["profile.json"], &raw); err != nil {
		t.Fatalf("profile.json is not valid JSON: %v", err)
	}
	if _, ok := raw["settings"]; ok {
		t.Fatal("profile.json must omit settings: the launcher's ProfileSettings requires memory")
	}
	for _, key := range []string{"id", "name", "path", "game_version", "loader", "loader_version", "banner", "mods"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("profile.json is missing %q", key)
		}
	}

	var parsed struct {
		GameVersion string `json:"game_version"`
		Loader      string `json:"loader"`
		Mods        []struct {
			AssociatedLoader string `json:"associated_loader"`
			UpdatesEnabled   *bool  `json:"updates_enabled"`
			Source           struct {
				Type        string `json:"type"`
				ProjectID   string `json:"project_id"`
				VersionID   string `json:"version_id"`
				FileName    string `json:"file_name"`
				DownloadURL string `json:"download_url"`
				FileHash    string `json:"file_hash_sha1"`
			} `json:"source"`
		} `json:"mods"`
		Banner struct {
			Source struct {
				Type     string `json:"type"`
				Data     string `json:"data"`
				MimeType string `json:"mimeType"`
			} `json:"source"`
		} `json:"banner"`
	}
	if err := json.Unmarshal(entries["profile.json"], &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.GameVersion != "26.2" || parsed.Loader != "fabric" {
		t.Fatalf("unexpected loader fields: %+v", parsed)
	}
	if parsed.Banner.Source.Type != "base64" || parsed.Banner.Source.MimeType != "image/png" || parsed.Banner.Source.Data != "AAAA" {
		t.Fatalf("banner would be stripped or not render: %+v", parsed.Banner.Source)
	}
	if len(parsed.Mods) != 1 {
		t.Fatalf("mods = %d", len(parsed.Mods))
	}
	mod := parsed.Mods[0]
	if mod.AssociatedLoader != "fabric" {
		t.Fatalf("associated_loader = %q", mod.AssociatedLoader)
	}
	if mod.UpdatesEnabled == nil || *mod.UpdatesEnabled {
		t.Fatal("updates_enabled must be written explicitly as false")
	}
	if mod.Source.Type != "modrinth" || mod.Source.FileHash != "deadbeef" || mod.Source.DownloadURL == "" {
		t.Fatalf("unexpected source: %+v", mod.Source)
	}
}

func TestWriteArchiveRemovesPartialFile(t *testing.T) {
	out := filepath.Join(t.TempDir(), "pack.noriskpack")
	err := writeArchive(out, func(zw *zip.Writer) error {
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatalf("partial archive left behind at %s", out)
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
