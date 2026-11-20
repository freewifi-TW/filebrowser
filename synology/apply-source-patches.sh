#!/usr/bin/env bash
# Applies the fork's one-line hooks to an upstream source tree at build time.
#
#   synology/apply-source-patches.sh <path to upstream checkout>
#
# The fork never commits changes to upstream files. The few places where upstream
# code has to call into the overlay are inserted here instead, each next to an
# anchor line that must exist exactly once. When upstream changes such a place the
# hook is skipped with a warning (the feature is then off in that build) rather than
# breaking the package build.
set -euo pipefail

src="${1:?usage: apply-source-patches.sh <upstream checkout>}"
applied=0
skipped=0

warn() {
    echo "::warning::$*"
    echo "apply-source-patches: $*" >&2
}

# insert_before <file> <exact anchor line> <line to insert> <feature>
insert_before() {
    local file="${src}/$1" anchor="$2" insert="$3" feature="$4"
    if [ ! -f "${file}" ]; then
        warn "${feature}: $1 not found, hook skipped"
        skipped=$((skipped + 1))
        return 0
    fi
    if grep -qxF -- "${insert}" "${file}"; then
        applied=$((applied + 1)) # already applied
        return 0
    fi
    local count
    count="$(grep -cxF -- "${anchor}" "${file}" || true)"
    if [ "${count}" != "1" ]; then
        warn "${feature}: anchor found ${count} times in $1 (expected 1), hook skipped"
        skipped=$((skipped + 1))
        return 0
    fi
    ANCHOR="${anchor}" INSERT="${insert}" perl -i -ne '
        chomp(my $line = $_);
        $line =~ s/\r$//;
        print "$ENV{INSERT}\n" if $line eq $ENV{ANCHOR};
        print $_;
    ' "${file}"
    grep -qxF -- "${insert}" "${file}" || { warn "${feature}: insertion into $1 failed"; skipped=$((skipped + 1)); return 0; }
    applied=$((applied + 1))
    echo "apply-source-patches: ${feature}: hooked into $1"
}

# Playback of formats browsers cannot decode: serve the converted MP4 instead.
# Inserted in ServeSingleFile, after all access checks and right before the file is opened.
insert_before "backend/internal/web/stream.go" \
    $'\tfd, err := os.Open(realPath)' \
    $'\trealPath, displayFileName = synologyPlayable(r, realPath, displayFileName)' \
    "video conversion"

echo "apply-source-patches: ${applied} applied, ${skipped} skipped"
