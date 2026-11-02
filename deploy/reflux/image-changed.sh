#!/bin/sh
# Whether an image's content changed between two commits, so a push to main
# that does not touch it builds no new image: a new build has a new digest
# (its labels name the commit and the time), and the owner's bot offers
# every new digest as a server update.
#
#   deploy/reflux/image-changed.sh node|egress <before> <after>
#
# Prints "true" or "false". The inputs are the Go files and embedded files
# of the image's binary in this module (go list -deps, as the Dockerfile
# builds it), go.mod/go.sum, the image's own files and the workflow. Without
# a usable <before> (a new branch, a rewritten history) it says "true".
set -eu

image=$1 before=$2 after=$3

case $image in
node)
	pkg=.
	own="Dockerfile docker/ .dockerignore"
	;;
egress)
	pkg=./cmd/reflux-egress
	own="deploy/reflux/egress/"
	;;
*)
	echo "unknown image: $image" >&2
	exit 2
	;;
esac

if ! git cat-file -e "$before^{commit}" 2>/dev/null; then
	echo true
	exit 0
fi

changed=$(git diff --name-only "$before" "$after")
[ -n "$changed" ] || { echo false; exit 0; }

root=$(pwd)
inputs=$(CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go list -deps -f \
	'{{if and .Module .Module.Main}}{{$d := .Dir}}{{range .GoFiles}}{{$d}}/{{.}}
{{end}}{{range .EmbedFiles}}{{$d}}/{{.}}
{{end}}{{end}}' "$pkg" | sed "s|^$root/||" | sort -u)

for f in $changed; do
	case $f in
	go.mod | go.sum | .github/workflows/reflux-images.yml | deploy/reflux/image-changed.sh)
		echo true
		exit 0
		;;
	esac
	for o in $own; do
		case $f in
		"$o" | "$o"*)
			echo true
			exit 0
			;;
		esac
	done
	if printf '%s\n' "$inputs" | grep -qxF "$f"; then
		echo true
		exit 0
	fi
done
echo false
