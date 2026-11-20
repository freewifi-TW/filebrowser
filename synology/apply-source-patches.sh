#!/usr/bin/env bash
# Applies the fork's one-line hooks to an upstream source tree at build time.
#
#   synology/apply-source-patches.sh <path to upstream checkout>
#
# The fork never commits changes to upstream files. The few places where upstream
# code has to call into the overlay are patched here instead, each identified by an
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

# count_line <file> <exact line>: how many lines equal it, ignoring a trailing CR.
count_line() {
    NEEDLE="$2" perl -ne 's/\r?\n$//; $n++ if $_ eq $ENV{NEEDLE}; END { print $n // 0 }' "$1"
}

# patch_line <insert|replace> <file> <exact anchor line> <new line> <feature>
#   insert:  put <new line> in front of the anchor line
#   replace: put <new line> in place of the anchor line
patch_line() {
    local mode="$1" rel="$2" anchor="$3" new="$4" feature="$5"
    local file="${src}/${rel}"
    if [ ! -f "${file}" ]; then
        warn "${feature}: ${rel} not found, hook skipped"
        skipped=$((skipped + 1))
        return 0
    fi
    if [ "$(count_line "${file}" "${new}")" != "0" ]; then
        applied=$((applied + 1)) # already applied
        return 0
    fi
    local count
    count="$(count_line "${file}" "${anchor}")"
    if [ "${count}" != "1" ]; then
        warn "${feature}: anchor found ${count} times in ${rel} (expected 1), hook skipped"
        skipped=$((skipped + 1))
        return 0
    fi
    MODE="${mode}" ANCHOR="${anchor}" NEW="${new}" perl -i -ne '
        my $line = $_;
        $line =~ s/\r?\n$//;
        if ($line eq $ENV{ANCHOR}) {
            print "$ENV{NEW}\n";
            print $_ if $ENV{MODE} eq "insert";
        } else {
            print $_;
        }
    ' "${file}"
    if [ "$(count_line "${file}" "${new}")" != "1" ]; then
        warn "${feature}: patching ${rel} failed"
        skipped=$((skipped + 1))
        return 0
    fi
    applied=$((applied + 1))
    echo "apply-source-patches: ${feature}: hooked into ${rel}"
}

TAB="$(printf '\t')"

# Playback of formats browsers cannot decode: serve the converted MP4 instead.
# Inserted in ServeSingleFile, after all access checks and right before the file is opened.
patch_line insert "backend/internal/web/stream.go" \
    "${TAB}fd, err := os.Open(realPath)" \
    "${TAB}realPath, displayFileName = synologyPlayable(r, realPath, displayFileName)" \
    "video conversion"

# Folder listings: remember ffprobe results instead of probing every video on every visit.
patch_line replace "backend/internal/adapters/fs/files/files.go" \
    "${TAB}info, err := service.ProbeFile(ctx, realPath)" \
    "${TAB}info, err := synologyCachedProbe(ctx, service, realPath)" \
    "media probe cache"

echo "apply-source-patches: ${applied} applied, ${skipped} skipped"
