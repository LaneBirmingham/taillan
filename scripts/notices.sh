#!/bin/sh
# Regenerates THIRD_PARTY_NOTICES.txt: the license of every module that is
# compiled into a TailLAN release binary (any OS), plus Go's own. Several,
# including tailcat and Tailscale, are BSD 3-Clause, which requires their
# notices to ship with binaries. Run via `make notices` after changing go.mod.
set -eu
export LC_ALL=C
GO=${GO:-go}
tags=$(cat build-tags.txt)
platforms=${PLATFORMS:-"linux/amd64 linux/arm64 windows/amd64 windows/arm64 darwin/amd64 darwin/arm64"}
out=THIRD_PARTY_NOTICES.txt
tmp=$(mktemp)
trap 'rm -f "$tmp" "$tmp.list"' EXIT

# "<module path>@<version> <dir holding a license file>" for every package
# of every module, checking the package's own directory and its parents up
# to the module root (some modules carry code under a different license).
for p in $platforms; do
	GOOS=${p%/*} GOARCH=${p#*/} $GO list -deps -tags "$tags" \
		-f '{{if and .Module (not .Module.Main)}}{{.Module.Path}}@{{.Module.Version}} {{with .Module.Replace}}{{.Dir}}{{else}}{{.Module.Dir}}{{end}} {{.Dir}}{{end}}' .
done | sort -u | while read -r mod root dir; do
	while :; do
		for f in "$dir"/LICENSE* "$dir"/LICENCE* "$dir"/COPYING* "$dir"/NOTICE*; do
			[ -f "$f" ] && echo "$mod ${f#"$root"/}"
		done
		[ "$dir" = "$root" ] && break
		dir=$(dirname "$dir")
	done
done | sort -u > "$tmp.list"

{
	echo "TailLAN is MIT licensed (see LICENSE). TailLAN binaries also contain the"
	echo "following open source software, under the licenses reproduced below."
	echo
	echo "================================================================================"
	echo "The Go programming language (standard library and runtime)"
	echo "LICENSE"
	echo "--------------------------------------------------------------------------------"
	cat "$($GO env GOROOT)/LICENSE"
	echo
	while read -r mod file; do
		path=${mod%@*}
		dir=$($GO list -m -f '{{with .Replace}}{{.Dir}}{{else}}{{.Dir}}{{end}}' "$path")
		echo "================================================================================"
		echo "$mod"
		echo "$file"
		echo "--------------------------------------------------------------------------------"
		cat "$dir/$file"
		echo
	done < "$tmp.list"
} > "$tmp"
mv "$tmp" "$out"
echo "wrote $out ($(grep -c '^====' "$out") components)"
