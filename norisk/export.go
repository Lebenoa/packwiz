package norisk

import (
	"archive/zip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/packwiz/packwiz/cmd"
	"github.com/packwiz/packwiz/cmdshared"
	"github.com/packwiz/packwiz/core"
	"github.com/packwiz/packwiz/curseforge"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var noriskCmd = &cobra.Command{
	Use:   "norisk",
	Short: "Export modpacks for the NoRisk launcher",
}

var exportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export the current modpack into a .noriskpack for the NoRisk launcher",
	Long: `Export the current modpack into a .noriskpack, the archive imported by the NoRisk
launcher (v3).

The archive contains a profile.json at its root describing the loader version, every
mod record and the profile icon, plus an overrides/ directory with the pack's
non-metadata files (config, options.txt, resource packs, ...). The launcher downloads
and verifies mods itself, so mod files are never bundled: it deletes jars it does not
track, and jars placed in overrides/mods are not loaded.

Every mod must have an HTTPS download URL. Files in mods/ become mod records; files in
other folders (resourcepacks/, shaderpacks/, ...) are placed into overrides/ at the same
path. Server-only files are left out, and optional mods are exported disabled unless
they are opted in by default.

The profile icon is set in pack.toml:

  [export.norisk]
  icon = "icon.png"   # a local image (embedded as base64), an https:// URL, or a data: URI

The launcher names the imported profile after the .noriskpack file, so use -o if you
need a specific name.`,
	Args: cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		if err := runExport(); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
	},
}

func init() {
	cmd.Add(noriskCmd)
	noriskCmd.AddCommand(exportCmd)

	exportCmd.Flags().StringP("output", "o", "", "The file to export the modpack to (defaults to <name>-<version>.noriskpack)")
	exportCmd.Flags().String("icon", "", "Profile icon: a local image path, an https URL, or a data: URI (overrides the pack.toml icon)")
	_ = viper.BindPFlag("norisk.export.output", exportCmd.Flags().Lookup("output"))
	_ = viper.BindPFlag("norisk.export.icon", exportCmd.Flags().Lookup("icon"))
}

func runExport() error {
	fmt.Println("Loading modpack...")
	pack, err := core.LoadPack()
	if err != nil {
		return err
	}
	index, err := pack.LoadIndex()
	if err != nil {
		return err
	}
	// Do a refresh to ensure files are up to date
	if err := index.Refresh(); err != nil {
		return err
	}
	if err := index.Write(); err != nil {
		return err
	}
	if err := pack.UpdateIndexHash(); err != nil {
		return err
	}
	if err := pack.Write(); err != nil {
		return err
	}

	loader, loaderVersion, err := loaderFor(pack)
	if err != nil {
		return err
	}
	mcVersion, err := pack.GetMCVersion()
	if err != nil {
		return err
	}

	banner, err := buildBanner(iconSpec(pack))
	if err != nil {
		return err
	}

	fmt.Println("Reading external files...")
	allMods, err := index.LoadAllMods()
	if err != nil {
		return fmt.Errorf("error reading file: %w", err)
	}

	// A NoRisk profile is a client instance: server-only files must not be installed.
	mods := make([]*core.Mod, 0, len(allMods))
	skippedServer := 0
	for _, mod := range allMods {
		if mod.Side == core.ServerSide {
			skippedServer++
			continue
		}
		mods = append(mods, mod)
	}
	if skippedServer > 0 {
		fmt.Printf("Skipping %v server-only file(s) from the client profile\n", skippedServer)
	}

	// Fail before downloading anything if a file cannot be represented in a profile.
	if err := checkRepresentable(mods); err != nil {
		return err
	}

	cfFiles, err := curseforge.ResolveModFiles(mods)
	if err != nil {
		return err
	}

	fileName := viper.GetString("norisk.export.output")
	if fileName == "" {
		fileName = pack.GetPackName() + ".noriskpack"
	}
	if !strings.HasSuffix(strings.ToLower(fileName), ".noriskpack") {
		fileName += ".noriskpack"
	}

	fmt.Printf("Retrieving %v external files...\n", len(mods))
	session, err := core.CreateDownloadSession(mods, []string{"sha1"})
	if err != nil {
		return fmt.Errorf("error retrieving external files: %w", err)
	}
	cmdshared.ListManualDownloads(session)

	profile := newProfile(pack, mcVersion, loader, loaderVersion, banner)

	var failures []string
	seenFileNames := make(map[string]string)
	bundled := 0

	err = writeArchive(fileName, func(zw *zip.Writer) error {
		// Add an overrides folder even if there are no files to go in it
		if _, err := zw.Create("overrides/"); err != nil {
			return fmt.Errorf("failed to add overrides folder: %w", err)
		}

		for dl := range session.StartDownloads() {
			if dl.Error != nil {
				failures = append(failures, fmt.Sprintf("%s (%s): %v", dl.Mod.Name, dl.Mod.FileName, dl.Error))
				continue
			}
			for _, warning := range dl.Warnings {
				fmt.Printf("Warning for %s (%s): %v\n", dl.Mod.Name, dl.Mod.FileName, warning)
			}

			relPath, err := index.RelIndexPath(dl.Mod.GetDestFilePath())
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s (%s): %v", dl.Mod.Name, dl.Mod.FileName, err))
				_ = dl.File.Close()
				continue
			}

			if !isModRecordPath(relPath) {
				// Resource packs, shader packs and similar are plain overrides: the
				// launcher only manages the mods folder.
				if !cmdshared.AddToZip(dl, zw, "overrides", &index) {
					failures = append(failures, fmt.Sprintf("%s (%s): failed to add to archive", dl.Mod.Name, dl.Mod.FileName))
				} else {
					bundled++
				}
				continue
			}

			record, err := buildModRecord(dl, cfFiles, mcVersion, loader)
			_ = dl.File.Close()
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s (%s): %v", dl.Mod.Name, dl.Mod.FileName, err))
				continue
			}
			// Case-insensitive: the launcher's flat mods folder collides on Windows.
			nameKey := strings.ToLower(record.Source.FileName)
			if previous, duplicate := seenFileNames[nameKey]; duplicate {
				failures = append(failures, fmt.Sprintf(
					"%s (%s): duplicate mod file name %q, also provided by %s; the launcher installs all mods into one folder, so rename one of them",
					dl.Mod.Name, dl.Mod.FileName, record.Source.FileName, previous))
				continue
			}
			seenFileNames[nameKey] = relPath
			profile.Mods = append(profile.Mods, record)
			fmt.Printf("%s (%s) added to profile\n", dl.Mod.Name, dl.Mod.FileName)
		}

		if err := session.SaveIndex(); err != nil {
			return fmt.Errorf("error saving cache index: %w", err)
		}
		if len(failures) > 0 {
			return fmt.Errorf("export aborted, %v file(s) could not be included:\n  %s",
				len(failures), strings.Join(failures, "\n  "))
		}
		if bundled > 0 {
			fmt.Printf("%v non-mod file(s) added to overrides\n", bundled)
		}

		// Ordinary files (config, options.txt, loose resource packs, ...) are plain
		// overrides that the launcher extracts into the instance.
		cmdshared.AddNonMetafileOverrides(&index, zw)

		// Sort for reproducible output
		sort.Slice(profile.Mods, func(i, j int) bool {
			return profile.Mods[i].Source.FileName < profile.Mods[j].Source.FileName
		})
		if err := writeProfileJSON(zw, profile); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}

	fmt.Printf("Exported %v mods\n", len(profile.Mods))
	if banner == nil {
		fmt.Println("No profile icon configured; set [export.norisk] icon in pack.toml to add one")
	}
	fmt.Println("Modpack exported to " + fileName)
	return nil
}

// iconSpec finds the configured icon, preferring the command line flag.
func iconSpec(pack core.Pack) string {
	if spec := viper.GetString("norisk.export.icon"); spec != "" {
		return spec
	}
	if cfg, ok := pack.Export["norisk"]; ok {
		if spec, ok := cfg["icon"].(string); ok {
			return spec
		}
	}
	return ""
}

// newProfile fills in the profile fields that do not depend on downloaded files.
func newProfile(pack core.Pack, mcVersion, loader, loaderVersion string, banner *ProfileBanner) Profile {
	name := strings.TrimSpace(pack.Name)
	if name == "" {
		name = "pack"
	}
	profilePath := core.SlugifyName(name)
	if profilePath == "" {
		profilePath = "pack"
	}
	return Profile{
		ID:            stableUUID("profile", name),
		Name:          name,
		Path:          profilePath,
		GameVersion:   mcVersion,
		Loader:        loader,
		LoaderVersion: loaderVersion,
		Description:   pack.Description,
		Group:         modpackGroup,
		Banner:        banner,
		Mods:          make([]Mod, 0),
	}
}

// checkRepresentable rejects files the launcher could not install before any download
// starts, so a failed export does not leave a half-written archive behind.
func checkRepresentable(mods []*core.Mod) error {
	for _, mod := range mods {
		if _, err := modFileName(mod); err != nil {
			return err
		}
		if mod.Download.Mode == core.ModeURL || mod.Download.Mode == "" {
			u, err := url.Parse(strings.TrimSpace(mod.Download.URL))
			if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" {
				return fmt.Errorf(
					"%s (%s) does not have an HTTPS download URL (%q); the NoRisk launcher only downloads files over HTTPS, so upload the file or switch it to a supported metadata source",
					mod.Name, mod.FileName, mod.Download.URL)
			}
		}
	}
	return nil
}

// isModRecordPath reports whether a pack-relative path belongs in the profile's mod
// list rather than in overrides/. Overrides are extracted into the instance as-is, and
// the launcher ignores jars under overrides/mods, so mods must be records.
//
// The launcher installs mods into a single flat folder, so any depth under mods/ is a
// record (the file name is used, see modFileName).
func isModRecordPath(relPath string) bool {
	clean := path.Clean(relPath)
	ext := strings.ToLower(path.Ext(clean))
	firstSegment, _, _ := strings.Cut(clean, "/")
	if strings.EqualFold(firstSegment, "mods") {
		return ext == ".jar" || ext == ".zip"
	}
	// Legacy packs keep mod metadata at the pack root; those files are mods too.
	return path.Dir(clean) == "." && ext == ".jar"
}

// buildModRecord converts a completed download into the mod record the launcher
// persists in the profile.
func buildModRecord(dl core.CompletedDownload, cfFiles map[uint32]curseforge.FileInfo, mcVersion, loader string) (Mod, error) {
	mod := dl.Mod

	downloadURL, err := modDownloadURL(mod, cfFiles)
	if err != nil {
		return Mod{}, err
	}
	parsed, err := url.Parse(downloadURL)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" {
		return Mod{}, fmt.Errorf("download URL is not a valid HTTPS URL: %q", downloadURL)
	}
	fileName, err := modFileName(mod)
	if err != nil {
		return Mod{}, err
	}

	source := ModSource{FileName: fileName}
	host := strings.ToLower(parsed.Hostname())
	projectID := updateString(mod, "modrinth", "mod-id")
	versionID := updateString(mod, "modrinth", "version")
	cfProjectID := updateString(mod, "curseforge", "project-id")
	cfFileID := updateString(mod, "curseforge", "file-id")

	switch {
	case host == "cdn.modrinth.com" && projectID != "" && versionID != "":
		source.Type = sourceModrinth
		source.ProjectID = projectID
		source.VersionID = versionID
		source.DownloadURL = downloadURL
		source.FileHash = dl.Hashes["sha1"]
	case isCurseForgeHost(host) && cfProjectID != "" && cfFileID != "":
		source.Type = sourceCurseForge
		source.ProjectID = cfProjectID
		source.FileID = cfFileID
		source.DownloadURL = downloadURL
		source.FileHash = dl.Hashes["sha1"]
		// The launcher recognises CurseForge files by fingerprint, not by SHA-1. Only
		// the value CurseForge returned for this file is used; it is never derived.
		if fileID, err := curseforge.GetFileID(mod); err == nil {
			source.Fingerprint = cfFiles[fileID].Fingerprint
		}
	default:
		// Any other HTTPS host is fine; the launcher reports it as a third-party host.
		source.Type = sourceURL
		source.URL = downloadURL
	}

	displayName := mod.Name
	if strings.TrimSpace(displayName) == "" {
		displayName = fileName
	}
	record := Mod{
		ID:               stableUUID("mod", fileName, downloadURL),
		Source:           source,
		Enabled:          !(mod.Option != nil && mod.Option.Optional && !mod.Option.Default),
		DisplayName:      displayName,
		GameVersions:     []string{mcVersion},
		AssociatedLoader: loader,
		UpdatesEnabled:   false,
	}

	switch source.Type {
	case sourceModrinth:
		record.ModpackOrigin = "modrinth:" + projectID + ":" + versionID
	case sourceCurseForge:
		record.ModpackOrigin = "curseforge:" + cfProjectID + ":" + cfFileID
	}
	return record, nil
}

func modDownloadURL(mod *core.Mod, cfFiles map[uint32]curseforge.FileInfo) (string, error) {
	switch mod.Download.Mode {
	case core.ModeURL, "":
		return strings.TrimSpace(mod.Download.URL), nil
	case core.ModeCF:
		raw := updateString(mod, "curseforge", "file-id")
		id, err := strconv.ParseUint(raw, 10, 32)
		if err != nil {
			return "", fmt.Errorf("invalid CurseForge file ID %q", raw)
		}
		file, ok := cfFiles[uint32(id)]
		if !ok || file.DownloadURL == "" {
			return "", errors.New("CurseForge did not return a download URL for this file (the project may have third-party distribution disabled)")
		}
		return file.DownloadURL, nil
	default:
		return "", fmt.Errorf("unsupported download mode %q", mod.Download.Mode)
	}
}

func isCurseForgeHost(host string) bool {
	switch host {
	case "edge.forgecdn.net", "mediafilez.forgecdn.net", "media.forgecdn.net":
		return true
	}
	return false
}

func writeProfileJSON(zw *zip.Writer, profile Profile) error {
	w, err := zw.Create("profile.json")
	if err != nil {
		return fmt.Errorf("failed to create profile.json: %w", err)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(profile); err != nil {
		return fmt.Errorf("failed to write profile.json: %w", err)
	}
	return nil
}

// writeArchive writes the archive via a temporary file, so a failed export never
// leaves a partially written .noriskpack where the real one used to be.
func writeArchive(fileName string, write func(*zip.Writer) error) (err error) {
	outFile, err := os.CreateTemp(filepath.Dir(fileName), ".noriskpack-*")
	if err != nil {
		return fmt.Errorf("failed to create zip: %w", err)
	}
	defer func() {
		if err != nil {
			_ = outFile.Close()
			_ = os.Remove(outFile.Name())
		}
	}()

	zw := zip.NewWriter(outFile)
	if err := write(zw); err != nil {
		_ = zw.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("error writing export file: %w", err)
	}
	if err := outFile.Close(); err != nil {
		return fmt.Errorf("error writing export file: %w", err)
	}
	if err := os.Rename(outFile.Name(), fileName); err != nil {
		_ = os.Remove(outFile.Name())
		return fmt.Errorf("error writing export file: %w", err)
	}
	return nil
}

const maxIconBytes = 2 << 20

var iconMimeTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// buildBanner turns the configured icon into the only banner forms the importer keeps:
// a Base64 payload or an https:// URL. Local image files are embedded so the icon
// travels with profile.json and cannot be stripped as a launcher-relative path.
func buildBanner(spec string) (*ProfileBanner, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}

	lower := strings.ToLower(spec)
	switch {
	case strings.HasPrefix(lower, "data:"):
		return bannerFromDataURI(spec)
	case strings.HasPrefix(lower, "https://"):
		parsed, err := url.Parse(spec)
		if err != nil || parsed.Host == "" {
			return nil, fmt.Errorf("icon URL is not valid: %q", spec)
		}
		return &ProfileBanner{Source: ImageSource{Type: "url", URL: spec}}, nil
	case strings.HasPrefix(lower, "http://"):
		return nil, errors.New("icon URLs must use https://; the NoRisk importer strips insecure image URLs")
	case strings.Contains(spec, "://"):
		return nil, fmt.Errorf("unsupported icon URL %q; use an https:// URL or a local image file", spec)
	}

	iconPath := spec
	if !filepath.IsAbs(iconPath) {
		iconPath = filepath.Join(filepath.Dir(viper.GetString("pack-file")), filepath.FromSlash(spec))
	}
	mimeType, ok := iconMimeTypes[strings.ToLower(filepath.Ext(iconPath))]
	if !ok {
		return nil, fmt.Errorf("icon %s must be a png, jpg, jpeg, gif or webp image", iconPath)
	}
	data, err := os.ReadFile(iconPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read icon: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("icon %s is empty", iconPath)
	}
	if len(data) > maxIconBytes {
		return nil, fmt.Errorf("icon %s is larger than %v MiB", iconPath, maxIconBytes>>20)
	}
	return &ProfileBanner{Source: ImageSource{
		Type:     "base64",
		Data:     base64.StdEncoding.EncodeToString(data),
		MimeType: mimeType,
	}}, nil
}

func bannerFromDataURI(spec string) (*ProfileBanner, error) {
	rest := spec[len("data:"):]
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return nil, errors.New("icon data URI is missing a comma separator")
	}
	meta, data := rest[:comma], rest[comma+1:]
	if !strings.HasSuffix(strings.ToLower(meta), ";base64") {
		return nil, errors.New("icon data URI must be base64 encoded")
	}
	mimeType := meta[:len(meta)-len(";base64")]
	if mimeType == "" {
		mimeType = "image/png"
	}
	if !strings.HasPrefix(strings.ToLower(mimeType), "image/") {
		return nil, fmt.Errorf("icon data URI has a non-image MIME type: %q", mimeType)
	}
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, fmt.Errorf("icon data URI is not valid base64: %w", err)
	}
	if len(decoded) == 0 {
		return nil, errors.New("icon data URI contains no image data")
	}
	return &ProfileBanner{Source: ImageSource{
		Type:     "base64",
		Data:     data,
		MimeType: mimeType,
	}}, nil
}
