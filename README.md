# packwiz
packwiz is a command line tool for creating Minecraft modpacks. Instead of managing JAR files directly, packwiz creates TOML metadata files which can be easily version-controlled and shared with git (see an example pack [here](https://github.com/packwiz/packwiz-example-pack)). You can then [export it to a CurseForge or Modrinth modpack](https://packwiz.infra.link/tutorials/hosting/curseforge/), or [use packwiz-installer](https://packwiz.infra.link/tutorials/installing/packwiz-installer/) for an auto-updating MultiMC instance.

packwiz is great for...

- Distributing private modpacks for servers
- Creating modpacks for CurseForge and Modrinth

packwiz is not so great for...

- Managing downloaded mod files (use [Curse/GDLauncher or another CLI](https://gist.github.com/comp500/13ae6f058221196077fb19953ac608c7))
- People who want a GUI (though there are some [third-party efforts](https://github.com/ExoPlant/packwiz-gui))

Join my Discord server if you need help [here](https://discord.gg/Csh8zbbhCt)!

## Different from upstream

This fork adds the following on top of upstream packwiz:

- **Downloads for distribution-disabled CurseForge projects** — uses the trusted
  launcher API key (MultiMC/PolyMC/Prism family), so mods whose authors disable
  third-party distribution (e.g. InstaPack) resolve and download through the API
  instead of falling back to manual downloads and aborting exports. See
  [CurseForge API key](#curseforge-api-key).
- **NoRisk launcher export** — `packwiz norisk export` produces a `.noriskpack`
  for the [NoRisk launcher](https://github.com/NoRiskClient/noriskclient-launcher)
  (root `profile.json` plus `overrides/`, icon embedding, CurseForge-backed mod
  records). See [NoRisk launcher export](#norisk-launcher-export).
- **Unbundled Modrinth export** — `packwiz modrinth export --no-bundle` lists every
  mod in `modrinth.index.json` by download URL (including CurseForge CDN links) instead
  of embedding the jars in overrides. See
  [Unbundled .mrpack export](#unbundled-mrpack-export).

## CurseForge API key

packwiz sends requests to the CurseForge API using the public API key shared by the
MultiMC/PolyMC/Prism launcher family (see
[enjarai/the-curseforge-api-key](https://github.com/enjarai/the-curseforge-api-key)).

This key is required for more than just authentication: CurseForge only includes
`downloadUrl` in its API responses for projects that disabled third-party distribution
(`allowModDistribution = false`, e.g. [InstaPack](https://www.curseforge.com/minecraft/mc-mods/instapack))
when the request uses an API key it trusts, such as those from known launchers. With an
untrusted key those files come back without a `downloadUrl`, so packwiz would mark them
as manual downloads and exports would fail instead of downloading them.

If you fork or derive from packwiz, obtain your own key from
[console.curseforge.com](https://console.curseforge.com/) and build with
`-ldflags="-X 'github.com/packwiz/packwiz/curseforge.cfApiKey=key'"`. Note that custom
keys may not receive `downloadUrl` for distribution-disabled projects, in which case
those mods fall back to manual downloads.

## NoRisk launcher export

`packwiz norisk export` writes a `.noriskpack` for the
[NoRisk launcher](https://github.com/NoRiskClient/noriskclient-launcher): a root
`profile.json` describing the loader and every mod, plus an `overrides/` directory with the
pack's ordinary files (`config/`, `options.txt`, loose resource packs, ...).

The profile icon is embedded in `profile.json` and survives import. Configure it in
`pack.toml`:

```toml
[export.norisk]
icon = "icon.png"
```

`icon` may be a png/jpg/gif/webp file relative to `pack.toml` (embedded as Base64, up to
2 MiB), an `https://` URL, or a `data:image/...;base64,...` URI. `--icon` overrides it for
a single run, and `-o` changes the output path (a `.noriskpack` extension is added if
missing).

Mods are exported as download records, never as bundled jars: the launcher deletes mod
files it does not track and does not load jars under `overrides/mods`. Every mod therefore
needs an HTTPS download URL; CurseForge metadata mods are resolved through the CurseForge
API (including projects with third-party distribution disabled), and anything else becomes
a plain URL record. Files under `mods/` become mod records, files elsewhere are placed in
`overrides/` at the same path. Server-only files are left out, optional mods are exported
disabled unless `default = true`, and mod updates are disabled so the pack stays
authoritative.

CurseForge records also carry `file_fingerprint`, the murmur2 fingerprint CurseForge
reports for that file. The launcher's import preview matches CurseForge records to the
platform by this value rather than by SHA-1, so a record without it is listed as having no
checksum to check. The SHA-1 is still exported for file verification, and when CurseForge
returns no fingerprint the field is omitted instead of being derived from the SHA-1, so
the preview keeps reporting honestly rather than showing a fabricated match.

The launcher names the imported profile after the `.noriskpack` file, so use `-o` when you
need a specific name. It also extracts `overrides/` once at import time, so player changes
to those files are not overwritten later; re-import the pack after changing overrides.

## Unbundled .mrpack export

`packwiz modrinth export` normally lists files from Modrinth's allowed download domains
(`cdn.modrinth.com`, `github.com`, `raw.githubusercontent.com`, `gitlab.com`) in
`modrinth.index.json`, and embeds everything else — CurseForge mods, files on other hosts
— in `overrides/`, `client-overrides/` or `server-overrides/`.

`--no-bundle` lists every mod file in the manifest by URL instead:

```sh
packwiz modrinth export --no-bundle -o pack.mrpack
```

CurseForge metadata mods are resolved through the CurseForge API, including projects with
third-party distribution disabled (see [CurseForge API key](#curseforge-api-key)). The
resulting `edge.forgecdn.net` link needs no credentials and contains no API key; it is a
plain CDN URL that redirects once to `mediafilez.forgecdn.net`. Hashes and sizes come from
the file packwiz downloads for the export, so they describe exactly what a launcher will
verify.

`--no-bundle` ignores `--restrictDomains`. Nothing is omitted silently: a file without an
HTTPS URL (`http://`, `file://`, or a CurseForge file the API will not hand out) stops the
export with an error naming the file. The archive is written to a temporary file and moved
into place, so a failed export neither replaces an existing `.mrpack` nor leaves a
truncated one.

### Launcher compatibility

The flag is opt-in because support varies. What has been verified here: the generated
`.mrpack` contains valid entries, and each URL downloads anonymously with a SHA-1, SHA-512
and size matching the manifest. What has not: importing the result in a real launcher.

- **Modrinth** rejects packs whose `downloads` are not from its four allowed domains, so
  `--no-bundle` output that references CurseForge or other hosts cannot be uploaded. The
  command warns when it emits such a host.
- **Other launchers** must follow the CurseForge redirect (the format requires following
  redirects), and some enforce their own domain allowlists. Import the export in the
  launcher you target before relying on it.

## Features
- Git-friendly TOML-based metadata format
- MultiMC pack installer/updater, with support for optional mods and fast automatic updates - perfect for servers!
- Pack distribution with HTTP servers, with a built in local server for testing
- Easy installation and updating of multiple mods at once from CurseForge and Modrinth
- Exporting to CurseForge, Modrinth and NoRisk launcher packs
- Importing from CurseForge packs
- Server-only and Client-only mod handling
- Creation of remote file metadata from JAR files for CurseForge mods

## Installation
Prebuilt binaries are available from [GitHub Actions](https://github.com/packwiz/packwiz/actions) - the UI is a bit terrible, but essentially select the top build, then download the artifact ZIP for your system at the bottom of the page.  

Another option is to use [nightly.link](https://nightly.link/packwiz/packwiz/workflows/go/main). Just go to the page, and download the artifact for your system.  

To run the executable, first extract it, then add the folder where you extracted it to your PATH environment variable ([see tutorial for Windows here](https://www.howtogeek.com/118594/how-to-edit-your-system-path-for-easy-command-line-access/)) or move it to where you want to use it.

In future I will have a lot more installation options, but you can also compile from source:

1. Install Go (1.24 or newer) from https://golang.org/dl/
2. Run `go install github.com/packwiz/packwiz@latest`. Be patient, it has to download and compile dependencies as well!

## Documentation
See https://packwiz.infra.link/ for the full packwiz documentation!
