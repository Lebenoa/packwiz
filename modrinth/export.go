package modrinth

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/packwiz/packwiz/cmdshared"
	"github.com/packwiz/packwiz/curseforge"
	"github.com/spf13/viper"

	"github.com/packwiz/packwiz/core"
	"github.com/spf13/cobra"
)

// maxManifestFileSize is the largest size the manifest's 32-bit fileSize field holds.
const maxManifestFileSize = 1<<32 - 1

// exportCmd represents the export command
var exportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export the current modpack into a .mrpack for Modrinth",
	Long: `Export the current modpack into a .mrpack for Modrinth.

By default, files from the download domains Modrinth allows (see --restrictDomains) are
listed in the manifest by URL, and everything else is bundled into overrides/ or
client-overrides/.

With --no-bundle, every mod file is listed in the manifest by URL instead, using the
CurseForge CDN URL for CurseForge metadata mods. Those files are not embedded in the
archive, so the pack stays small, but any launcher that imports it has to accept those
URLs: Modrinth itself rejects packs whose downloads are not from its allowed domains,
and other launchers may enforce their own list. Nothing is ever omitted silently - the
export fails if a file cannot be referenced by URL.`,
	Args: cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("Loading modpack...")
		pack, err := core.LoadPack()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		index, err := pack.LoadIndex()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		// Do a refresh to ensure files are up to date
		err = index.Refresh()
		if err != nil {
			fmt.Println(err)
			return
		}
		err = index.Write()
		if err != nil {
			fmt.Println(err)
			return
		}
		err = pack.UpdateIndexHash()
		if err != nil {
			fmt.Println(err)
			return
		}
		err = pack.Write()
		if err != nil {
			fmt.Println(err)
			return
		}

		fmt.Println("Reading external files...")
		mods, err := index.LoadAllMods()
		if err != nil {
			fmt.Printf("Error reading file: %v\n", err)
			os.Exit(1)
		}

		fileName := viper.GetString("modrinth.export.output")
		if fileName == "" {
			fileName = pack.GetPackName() + ".mrpack"
		}

		noBundle := viper.GetBool("modrinth.export.noBundle")
		restrictDomains := viper.GetBool("modrinth.export.restrictDomains")

		// referenceByURL reports whether a file is listed in the manifest instead of
		// being bundled into an overrides folder.
		referenceByURL := func(mod *core.Mod) bool {
			return noBundle || canBeIncludedDirectly(mod, restrictDomains)
		}

		// Resolve everything the manifest will need before downloading anything, so that
		// --no-bundle fails fast instead of after downloading the whole pack.
		var cfURLs map[uint32]string
		if noBundle {
			cfURLs, err = curseforge.ResolveModDownloadURLs(mods)
			if err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
			if err = checkURLExportable(mods, cfURLs); err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
			fmt.Printf("Referencing %v mod file(s) by URL; no mod files will be bundled\n", len(mods))
		}

		fmt.Printf("Retrieving %v external files...\n", len(mods))

		session, err := core.CreateDownloadSession(mods, []string{"sha1", "sha512", "length-bytes"})
		if err != nil {
			fmt.Printf("Error retrieving external files: %v\n", err)
			os.Exit(1)
		}

		cmdshared.ListManualDownloads(session)

		if !noBundle {
			for _, mod := range mods {
				if !canBeIncludedDirectly(mod, restrictDomains) {
					cmdshared.PrintDisclaimer(false)
					break
				}
			}
		}

		outFile, err := os.CreateTemp(filepath.Dir(fileName), ".mrpack-*")
		if err != nil {
			fmt.Printf("Failed to create zip: %s\n", err.Error())
			os.Exit(1)
		}
		exp := zip.NewWriter(outFile)

		// fail reports an error and removes the temporary archive, so a failed export
		// never replaces an existing .mrpack or leaves a truncated one behind.
		fail := func(format string, args ...interface{}) {
			fmt.Printf(format+"\n", args...)
			_ = exp.Close()
			_ = outFile.Close()
			_ = os.Remove(outFile.Name())
			os.Exit(1)
		}

		// Add an overrides folder even if there are no files to go in it
		if _, err = exp.Create("overrides/"); err != nil {
			fail("Failed to add overrides folder: %s", err.Error())
		}

		manifestFiles := make([]PackFile, 0)
		var unbundleable []string

		for dl := range session.StartDownloads() {
			if !referenceByURL(dl.Mod) {
				if dl.Mod.Side == core.ClientSide {
					_ = cmdshared.AddToZip(dl, exp, "client-overrides", &index)
				} else if dl.Mod.Side == core.ServerSide {
					_ = cmdshared.AddToZip(dl, exp, "server-overrides", &index)
				} else {
					_ = cmdshared.AddToZip(dl, exp, "overrides", &index)
				}
				continue
			}

			if dl.Error != nil {
				if noBundle {
					// Files must not be dropped silently when every file is referenced
					// by URL: the pack would be missing a mod with no explanation.
					unbundleable = append(unbundleable, fmt.Sprintf("%s (%s): %v", dl.Mod.Name, dl.Mod.FileName, dl.Error))
				} else {
					fmt.Printf("Download of %s (%s) failed: %v\n", dl.Mod.Name, dl.Mod.FileName, dl.Error)
				}
				continue
			}
			for _, warning := range dl.Warnings {
				fmt.Printf("Warning for %s (%s): %v\n", dl.Mod.Name, dl.Mod.FileName, warning)
			}

			path, err := index.RelIndexPath(dl.Mod.GetDestFilePath())
			var manifestFile PackFile
			if err == nil {
				manifestFile, err = buildManifestFile(dl, path, cfURLs)
			}
			if dl.File != nil {
				_ = dl.File.Close()
			}
			if err != nil {
				if noBundle {
					unbundleable = append(unbundleable, fmt.Sprintf("%s (%s): %v", dl.Mod.Name, dl.Mod.FileName, err))
				} else {
					fmt.Printf("Error adding %s (%s) to manifest: %v\n", dl.Mod.Name, dl.Mod.FileName, err)
				}
				continue
			}

			manifestFiles = append(manifestFiles, manifestFile)
			fmt.Printf("%s (%s) added to manifest\n", dl.Mod.Name, dl.Mod.FileName)
		}

		// sort by `path` property before serialising to ensure reproducibility
		sort.Slice(manifestFiles, func(i, j int) bool {
			return manifestFiles[i].Path < manifestFiles[j].Path
		})

		err = session.SaveIndex()
		if err != nil {
			fail("Error saving cache index: %v", err)
		}

		if len(unbundleable) > 0 {
			fail("Export aborted: %v file(s) could not be included:\n  %s\nExport without --no-bundle to bundle these files into the archive instead.",
				len(unbundleable), strings.Join(unbundleable, "\n  "))
		}

		dependencies := make(map[string]string)
		dependencies["minecraft"], err = pack.GetMCVersion()
		if err != nil {
			fail("Error creating manifest: %s", err.Error())
		}
		if quiltVersion, ok := pack.Versions["quilt"]; ok {
			dependencies["quilt-loader"] = quiltVersion
		} else if fabricVersion, ok := pack.Versions["fabric"]; ok {
			dependencies["fabric-loader"] = fabricVersion
		} else if forgeVersion, ok := pack.Versions["forge"]; ok {
			dependencies["forge"] = forgeVersion
		} else if neoforgeVersion, ok := pack.Versions["neoforge"]; ok {
			dependencies["neoforge"] = neoforgeVersion
		}

		manifest := Pack{
			FormatVersion: 1,
			Game:          "minecraft",
			VersionID:     pack.Version,
			Name:          pack.Name,
			Summary:       pack.Description,
			Files:         manifestFiles,
			Dependencies:  dependencies,
		}

		if len(pack.Version) == 0 {
			fmt.Println("Warning: pack.toml version field must not be empty to create a valid Modrinth pack")
		}

		manifestFile, err := exp.Create("modrinth.index.json")
		if err != nil {
			fail("Error creating manifest: %s", err.Error())
		}

		w := json.NewEncoder(manifestFile)
		w.SetIndent("", "    ") // Documentation uses 4 spaces
		err = w.Encode(manifest)
		if err != nil {
			fail("Error writing manifest: %s", err.Error())
		}

		cmdshared.AddNonMetafileOverrides(&index, exp)

		err = exp.Close()
		if err != nil {
			fail("Error writing export file: %s", err.Error())
		}
		err = outFile.Close()
		if err != nil {
			fail("Error writing export file: %s", err.Error())
		}
		err = os.Rename(outFile.Name(), fileName)
		if err != nil {
			fail("Error writing export file: %s", err.Error())
		}

		if noBundle {
			warnNonModrinthHosts(manifestFiles)
		}

		fmt.Println("Modpack exported to " + fileName)
	},
}

var whitelistedHosts = []string{
	"cdn.modrinth.com",
	"github.com",
	"raw.githubusercontent.com",
	"gitlab.com",
}

func canBeIncludedDirectly(mod *core.Mod, restrictDomains bool) bool {
	if mod.Download.Mode == core.ModeURL || mod.Download.Mode == "" {
		if !restrictDomains {
			return true
		}

		modUrl, err := url.Parse(mod.Download.URL)
		if err == nil {
			if slices.Contains(whitelistedHosts, modUrl.Host) {
				return true
			}
		}
	}
	return false
}

// exportURL returns the HTTPS URL the manifest should reference a file by.
//
// URL-mode mods carry their own URL. CurseForge mods get the URL the API resolved for
// their file ID, which is a plain CDN link that needs no credentials and redirects to
// mediafilez.forgecdn.net.
func exportURL(mod *core.Mod, cfURLs map[uint32]string) (string, error) {
	var raw string
	switch mod.Download.Mode {
	case core.ModeURL, "":
		raw = strings.TrimSpace(mod.Download.URL)
	case core.ModeCF:
		fileID, err := curseforge.GetFileID(mod)
		if err != nil {
			return "", err
		}
		resolved, ok := cfURLs[fileID]
		if !ok || resolved == "" {
			return "", fmt.Errorf("CurseForge did not return a download URL for file %d "+
				"(the project's author has not allowed third-party downloads, or the file was removed); "+
				"this file can only be bundled", fileID)
		}
		raw = resolved
	default:
		return "", fmt.Errorf("unsupported download mode %q", mod.Download.Mode)
	}

	if raw == "" {
		return "", errors.New("no download URL is configured")
	}
	parsed, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" {
		return "", fmt.Errorf("download URL %q is not a valid HTTPS URL; the manifest only allows HTTPS downloads", raw)
	}
	return raw, nil
}

// checkURLExportable verifies that every file can be referenced by URL before any
// download starts, so --no-bundle fails fast with an actionable message.
func checkURLExportable(mods []*core.Mod, cfURLs map[uint32]string) error {
	for _, mod := range mods {
		if _, err := exportURL(mod, cfURLs); err != nil {
			return fmt.Errorf("%s (%s): %w", mod.Name, mod.FileName, err)
		}
	}
	return nil
}

// manifestEnv maps a mod's side and optional flag onto the manifest's environment
// values, which may only be required, optional or unsupported.
func manifestEnv(mod *core.Mod) (client, server string) {
	installed := "required"
	if mod.Option != nil && mod.Option.Optional {
		installed = "optional"
	}
	switch mod.Side {
	case core.ClientSide:
		return installed, "unsupported"
	case core.ServerSide:
		return "unsupported", installed
	default:
		return installed, installed
	}
}

// buildManifestFile converts a completed download into a files[] entry. The format
// requires SHA-1, SHA-512, a size and an HTTPS URL for every entry, so a missing value
// is reported instead of being written out for a launcher to trip over.
func buildManifestFile(dl core.CompletedDownload, relPath string, cfURLs map[uint32]string) (PackFile, error) {
	downloadURL, err := exportURL(dl.Mod, cfURLs)
	if err != nil {
		return PackFile{}, err
	}
	sha1, sha512, fileSize, err := manifestHashes(dl)
	if err != nil {
		return PackFile{}, err
	}
	if fileSize > maxManifestFileSize {
		return PackFile{}, fmt.Errorf("file is %d bytes, which does not fit the manifest's 32-bit fileSize field", fileSize)
	}

	// Modrinth URLs must be RFC3986
	encoded, err := core.ReencodeURL(downloadURL)
	if err != nil {
		fmt.Printf("Error re-encoding download URL: %s\n", err.Error())
		encoded = downloadURL
	}

	clientEnv, serverEnv := manifestEnv(dl.Mod)
	return PackFile{
		Path:      relPath,
		Hashes:    map[string]string{"sha1": sha1, "sha512": sha512},
		Env:       &PackEnv{Client: clientEnv, Server: serverEnv},
		Downloads: []string{encoded},
		FileSize:  uint32(fileSize),
	}, nil
}

// manifestHashes returns the SHA-1 and SHA-512 hashes and the size of a downloaded file.
// The download session normally supplies all three, but a file served from the cache
// (for example one that was placed there manually) may only carry the cache's SHA-256,
// so in that case the file is re-read rather than failing the export.
func manifestHashes(dl core.CompletedDownload) (sha1, sha512 string, fileSize uint64, err error) {
	sha1, sha512 = dl.Hashes["sha1"], dl.Hashes["sha512"]
	size, sizeErr := strconv.ParseUint(dl.Hashes["length-bytes"], 10, 64)
	if sha1 != "" && sha512 != "" && sizeErr == nil {
		return sha1, sha512, size, nil
	}
	if dl.File == nil {
		return "", "", 0, errors.New("the download session did not provide a SHA-1 hash, a SHA-512 hash and a size, and there is no file to read them from")
	}

	if _, err := dl.File.Seek(0, io.SeekStart); err != nil {
		return "", "", 0, fmt.Errorf("failed to re-read file for its hashes: %w", err)
	}
	sha1Hasher, err := core.GetHashImpl("sha1")
	if err != nil {
		return "", "", 0, err
	}
	sha512Hasher, err := core.GetHashImpl("sha512")
	if err != nil {
		return "", "", 0, err
	}
	written, err := io.Copy(io.MultiWriter(sha1Hasher, sha512Hasher), dl.File)
	if err != nil {
		return "", "", 0, fmt.Errorf("failed to read file for its hashes: %w", err)
	}
	return sha1Hasher.HashToString(sha1Hasher.Sum(nil)), sha512Hasher.HashToString(sha512Hasher.Sum(nil)), uint64(written), nil
}

// warnNonModrinthHosts reports download hosts that launchers may accept but Modrinth
// does not, so the author is not surprised when an upload is rejected.
func warnNonModrinthHosts(files []PackFile) {
	hosts := make(map[string]bool)
	for _, file := range files {
		for _, raw := range file.Downloads {
			parsed, err := url.Parse(raw)
			if err != nil {
				continue
			}
			if !slices.Contains(whitelistedHosts, parsed.Hostname()) {
				hosts[parsed.Hostname()] = true
			}
		}
	}
	if len(hosts) == 0 {
		return
	}
	list := make([]string, 0, len(hosts))
	for host := range hosts {
		list = append(list, host)
	}
	sort.Strings(list)
	fmt.Printf("Warning: %v download host(s) are not on Modrinth's allowed list (%s): %s\n",
		len(list), strings.Join(whitelistedHosts, ", "), strings.Join(list, ", "))
	fmt.Println("Modrinth will reject this pack if it is uploaded; it is only usable in launchers that accept these URLs.")
}

func init() {
	modrinthCmd.AddCommand(exportCmd)
	exportCmd.Flags().Bool("restrictDomains", true, "Restricts domains to those allowed by modrinth.com")
	exportCmd.Flags().StringP("output", "o", "", "The file to export the modpack to")
	exportCmd.Flags().Bool("no-bundle", false, "List every mod file in the manifest by download URL instead of bundling it into overrides (ignores --restrictDomains; the result is not uploadable to Modrinth and may not import in all launchers)")
	_ = viper.BindPFlag("modrinth.export.restrictDomains", exportCmd.Flags().Lookup("restrictDomains"))
	_ = viper.BindPFlag("modrinth.export.output", exportCmd.Flags().Lookup("output"))
	_ = viper.BindPFlag("modrinth.export.noBundle", exportCmd.Flags().Lookup("no-bundle"))
}
