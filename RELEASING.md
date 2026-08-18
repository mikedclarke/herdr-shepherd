# Releasing

The whole process runs locally from a Mac. **Never GitHub Actions** — blocked
account-wide. A release is a tag, four cross-compiled binaries, a checksum
file, and a GitHub release whose body is the changelog entry.

Before starting: the repo is public, so every commit must already be
publishable, and `sh scripts/check.sh` must be green.

1. **Bump the version in three places** (they must agree):
   - `main.go` — `const version`
   - `herdr-plugin.toml` — `version`
   - `CHANGELOG.md` — a `## [X.Y.Z] - YYYY-MM-DD` section for the release
2. **Commit and push** to `main`.
3. **Tag** (annotated) and push the tag:
   ```sh
   git tag -a vX.Y.Z -m "vX.Y.Z: one-line summary"
   git push origin vX.Y.Z
   ```
4. **Cross-compile the four targets** (pure Go, no cgo) and checksum them.
   `scripts/install.sh` expects archives named
   `herdr-shepherd_<version>_<os>_<arch>.tar.gz` (version without the leading
   `v`), each containing the bare `herdr-shepherd` binary at the top level,
   and a sums file called exactly `SHA256SUMS`:
   ```sh
   DIST=$(mktemp -d)
   for os in darwin linux; do for arch in amd64 arm64; do
     CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -o "$DIST/herdr-shepherd" .
     tar -czf "$DIST/herdr-shepherd_X.Y.Z_${os}_${arch}.tar.gz" -C "$DIST" herdr-shepherd
     rm "$DIST/herdr-shepherd"
   done; done
   (cd "$DIST" && shasum -a 256 herdr-shepherd_X.Y.Z_*.tar.gz > SHA256SUMS)
   ```
5. **Create the GitHub release** with the changelog section as the body:
   ```sh
   awk '/^## \[X.Y.Z\]/{flag=1; next} /^## \[/{flag=0} flag' CHANGELOG.md > "$DIST/notes.md"
   gh release create vX.Y.Z --title vX.Y.Z --notes-file "$DIST/notes.md" \
     "$DIST"/herdr-shepherd_X.Y.Z_*.tar.gz "$DIST/SHA256SUMS"
   ```
6. **Verify the download path** against the real release (this is what
   no-Go-toolchain machines run):
   ```sh
   INSTALL_DIR=$(mktemp -d) sh scripts/install.sh   # resolves latest, checks SHA256SUMS
   ```
   The installed binary's `version` must print X.Y.Z.
7. **Update the installed plugin** wherever it runs (there is no
   `herdr plugin update`; reinstalling keeps actions and history — they live
   in the plugin config/state dirs, not the plugin directory):
   ```sh
   herdr plugin install mikedclarke/herdr-shepherd --yes
   ```
   Then restart the daemon: `kill -TERM $(cat ~/.local/state/herdr/plugins/mikedclarke.herdr-shepherd/daemon.lock)`,
   and from a herdr pane in the plugin directory run
   `./bin/herdr-shepherd daemon --detach`. Confirm with `herdr-shepherd status`
   and `herdr-shepherd version`.
