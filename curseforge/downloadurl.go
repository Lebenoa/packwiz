package curseforge

import (
	"fmt"
	"strconv"

	"github.com/packwiz/packwiz/core"
)

// FileInfo is the subset of CurseForge file metadata that exporters referencing files
// by URL need.
type FileInfo struct {
	// DownloadURL is empty when the API did not return one, which happens when the file
	// cannot be downloaded on the user's behalf.
	DownloadURL string
	// Fingerprint is CurseForge's murmur2 fingerprint for the file, the value launchers
	// use to recognise which platform file a record came from. It is nil when the API
	// did not provide one; it is never derived from another hash.
	Fingerprint *uint32
}

// GetFileID returns the file ID stored in a mod's CurseForge update metadata.
func GetFileID(mod *core.Mod) (uint32, error) {
	raw := updateValueString(mod, "file-id")
	id, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s (%s) has an invalid CurseForge file ID %q", mod.Name, mod.FileName, raw)
	}
	return uint32(id), nil
}

// ResolveModFiles resolves the CurseForge file metadata of every CurseForge metadata
// mod. The download session keeps that metadata inside its own downloader, so exporters
// that reference files by URL look it up here in one batch instead.
//
// The API only exposes downloadUrl for projects with third-party distribution disabled
// to keys from trusted launchers; a missing URL means the file cannot be downloaded on
// the user's behalf. The metadata is returned to the caller only and is not cached, per
// the CurseForge API terms; callers that embed it (exporters) are responsible for that.
func ResolveModFiles(mods []*core.Mod) (map[uint32]FileInfo, error) {
	fileIDs, err := curseForgeFileIDs(mods)
	if err != nil {
		return nil, err
	}
	if len(fileIDs) == 0 {
		return map[uint32]FileInfo{}, nil
	}
	files, err := cfDefaultClient.getFileInfoMultiple(fileIDs)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve CurseForge file metadata: %w", err)
	}
	return fileInfoFromAPI(files), nil
}

// ResolveModDownloadURLs resolves the direct download URL of every CurseForge metadata
// mod, omitting files the API has no URL for.
func ResolveModDownloadURLs(mods []*core.Mod) (map[uint32]string, error) {
	files, err := ResolveModFiles(mods)
	if err != nil {
		return nil, err
	}
	urls := make(map[uint32]string, len(files))
	for id, info := range files {
		if info.DownloadURL != "" {
			urls[id] = info.DownloadURL
		}
	}
	return urls, nil
}

// fileInfoFromAPI converts the API response into the exported view. CurseForge returns
// fileFingerprint for every file it knows, so a zero value means the response did not
// carry one rather than a genuine fingerprint of zero.
func fileInfoFromAPI(files []modFileInfo) map[uint32]FileInfo {
	infos := make(map[uint32]FileInfo, len(files))
	for _, file := range files {
		info := FileInfo{DownloadURL: file.DownloadURL}
		if file.Fingerprint != 0 {
			fingerprint := file.Fingerprint
			info.Fingerprint = &fingerprint
		}
		infos[file.ID] = info
	}
	return infos
}

// curseForgeFileIDs collects the distinct file IDs of the CurseForge metadata mods.
func curseForgeFileIDs(mods []*core.Mod) ([]uint32, error) {
	var fileIDs []uint32
	seen := make(map[uint32]bool)
	for _, mod := range mods {
		if mod.Download.Mode != core.ModeCF {
			continue
		}
		fileID, err := GetFileID(mod)
		if err != nil {
			return nil, err
		}
		if !seen[fileID] {
			seen[fileID] = true
			fileIDs = append(fileIDs, fileID)
		}
	}
	return fileIDs, nil
}

// updateValueString reads a value out of a mod's [update] table. TOML decodes strings as
// string and positive integers (e.g. CurseForge IDs) as int64.
func updateValueString(mod *core.Mod, key string) string {
	table, ok := mod.Update["curseforge"]
	if !ok {
		return ""
	}
	switch v := table[key].(type) {
	case string:
		return v
	case int64:
		return strconv.FormatInt(v, 10)
	case int:
		return strconv.Itoa(v)
	case uint32:
		return strconv.FormatUint(uint64(v), 10)
	case uint:
		return strconv.FormatUint(uint64(v), 10)
	}
	return ""
}
