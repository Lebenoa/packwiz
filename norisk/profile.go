package norisk

import (
	"crypto/sha1"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/packwiz/packwiz/core"
)

// This file models the profile.json that the NoRisk launcher parses when importing
// a .noriskpack. Field names, enum values and required/optional fields mirror the
// launcher v3 sources:
//
//   - Profile/Mod/ModSource/ProfileBanner: src-tauri/src/state/profile_state.rs
//   - import sanitization:                 src-tauri/src/utils/import_safety.rs
//   - import behaviour:                    src-tauri/src/integrations/norisk_packs.rs
//
// Notes that are load-bearing for a pack that survives import:
//   - Mod sources are serde tagged with rename_all = "snake_case", so CurseForge is
//     "curse_forge".
//   - ImageSource uses rename_all = "camelCase", so the MIME field is "mimeType".
//   - ProfileSettings is not emitted at all; its memory field has no default, so an
//     empty object would fail to deserialize. Omitting "settings" uses launcher defaults.
//   - Mod.associated_loader is mandatory at launch: the resolver skips mods without it.
type Profile struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Path          string         `json:"path"`
	GameVersion   string         `json:"game_version"`
	Loader        string         `json:"loader"`
	LoaderVersion string         `json:"loader_version,omitempty"`
	Description   string         `json:"description,omitempty"`
	Group         string         `json:"group,omitempty"`
	Banner        *ProfileBanner `json:"banner,omitempty"`
	Mods          []Mod          `json:"mods"`
}

// ProfileBanner is the profile icon; the launcher renders its Source.
type ProfileBanner struct {
	Source ImageSource `json:"source"`
}

// ImageSource is only ever emitted as "url" or "base64", the two variants that
// import_safety keeps from untrusted packs.
type ImageSource struct {
	Type     string `json:"type"`
	URL      string `json:"url,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

// Mod is a single file record in the imported profile.
type Mod struct {
	ID               string    `json:"id"`
	Source           ModSource `json:"source"`
	Enabled          bool      `json:"enabled"`
	DisplayName      string    `json:"display_name,omitempty"`
	GameVersions     []string  `json:"game_versions,omitempty"`
	AssociatedLoader string    `json:"associated_loader"`
	ModpackOrigin    string    `json:"modpack_origin,omitempty"`
	// updates_enabled defaults to true when absent; pack files are owned by packwiz,
	// so it is always written explicitly as false.
	UpdatesEnabled bool `json:"updates_enabled"`
}

// ModSource is a flattened union of the launcher's ModSource variants.
type ModSource struct {
	Type string `json:"type"`
	// Modrinth and CurseForge
	ProjectID   string `json:"project_id,omitempty"`
	VersionID   string `json:"version_id,omitempty"`
	FileID      string `json:"file_id,omitempty"`
	FileName    string `json:"file_name,omitempty"`
	DownloadURL string `json:"download_url,omitempty"`
	FileHash    string `json:"file_hash_sha1,omitempty"`
	// Fingerprint is CurseForge's file fingerprint (murmur2). The launcher's import
	// preview matches CurseForge records to the platform by this value, not by SHA-1,
	// so a record without it is reported as having no checksum to check. It is omitted
	// when CurseForge did not provide one rather than being derived from another hash.
	Fingerprint *uint32 `json:"file_fingerprint,omitempty"`
	// Plain URL
	URL string `json:"url,omitempty"`
}

const (
	sourceModrinth   = "modrinth"
	sourceCurseForge = "curse_forge"
	sourceURL        = "url"
)

// Loader names accepted by the launcher's ModLoader enum.
const (
	loaderVanilla  = "vanilla"
	loaderFabric   = "fabric"
	loaderQuilt    = "quilt"
	loaderForge    = "forge"
	loaderNeoForge = "neoforge"
)

// modpackGroup matches the group the launcher itself gives packs imported from
// .mrpack files. "modpacks" is an isolated group, so the profile gets its own
// instance directory instead of the shared Minecraft folder.
const modpackGroup = "MODPACKS"

// stableUUID derives a deterministic, RFC 4122-shaped UUID so repeated exports of
// the same pack are byte-identical. The launcher regenerates the profile ID on
// import but keeps mod IDs.
func stableUUID(parts ...string) string {
	sum := sha1.Sum([]byte(strings.Join(parts, "\x00")))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50 // name-based version
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// loaderFor maps pack.toml [versions] to the launcher's loader fields.
func loaderFor(pack core.Pack) (loader string, loaderVersion string, err error) {
	if strings.TrimSpace(pack.Versions["minecraft"]) == "" {
		return "", "", errors.New("no minecraft version specified in modpack, but the NoRisk launcher requires one")
	}

	// Quilt before Fabric: a Quilt pack may list both, and Quilt is the actual loader.
	for _, name := range []string{loaderQuilt, loaderFabric, loaderNeoForge, loaderForge} {
		if v, ok := pack.Versions[name]; ok && strings.TrimSpace(v) != "" {
			return name, strings.TrimSpace(v), nil
		}
	}

	// Reject loaders the launcher cannot launch rather than silently exporting a
	// profile that would not start with these plugins/mods.
	var unsupported []string
	for name := range pack.Versions {
		switch name {
		case "minecraft", loaderQuilt, loaderFabric, loaderNeoForge, loaderForge:
		default:
			unsupported = append(unsupported, name)
		}
	}
	if len(unsupported) > 0 {
		return "", "", fmt.Errorf(
			"the NoRisk launcher only supports vanilla, Fabric, Quilt, Forge and NeoForge profiles, but this pack declares: %s",
			strings.Join(unsupported, ", "))
	}

	return loaderVanilla, "", nil
}

// modFileName returns the plain file name the launcher stores the mod under.
// packwiz filenames are pack-relative; the launcher requires a single path
// component and rejects anything that is not a .jar/.zip.
func modFileName(mod *core.Mod) (string, error) {
	name := path.Base(strings.ReplaceAll(mod.FileName, "\\", "/"))
	if name == "" || name == "." || name == "/" {
		return "", fmt.Errorf("%s has no filename", mod.Name)
	}
	if strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("%s has a filename with a path separator: %s", mod.Name, mod.FileName)
	}
	ext := strings.ToLower(path.Ext(name))
	if ext != ".jar" && ext != ".zip" {
		return "", fmt.Errorf("%s is not a .jar or .zip file (%s)", mod.Name, name)
	}
	return name, nil
}

// updateString reads a value out of a mod's [update] table. TOML decodes strings
// as string and positive integers (CurseForge IDs) as int64.
func updateString(mod *core.Mod, plugin, key string) string {
	table, ok := mod.Update[plugin]
	if !ok {
		return ""
	}
	switch v := table[key].(type) {
	case string:
		return v
	case int64:
		return fmt.Sprintf("%d", v)
	case int:
		return fmt.Sprintf("%d", v)
	case uint32:
		return fmt.Sprintf("%d", v)
	case uint:
		return fmt.Sprintf("%d", v)
	}
	return ""
}
