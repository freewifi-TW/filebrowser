#!/usr/bin/env bash
# Assembles a Synology DSM 7.2+ package (.spk) from pre-built binaries.
# Needs only bash, GNU tar and gzip - no Synology toolkit. ImageMagick is optional
# (icons are resized with it when available).
#
#   synology/build-spk.sh --arch amd64 --version 2.1.5-0001 --upstream-tag v2.1.5-beta \
#       --bin-dir dist/amd64 --icon src/frontend/public/img/icons/favicon.png --out-dir out
#
# --bin-dir must contain static linux binaries: filebrowser, ffmpeg, ffprobe.
# --variant alias    (default) also registers /filebrowser/ on DSM's own web server,
#                    which is what makes the app reachable through QuickConnect.
# --variant portonly leaves DSM's nginx alone; the app is only served on port 8580.
set -euo pipefail

PKG_NAME="FileBrowserQuantum"
PORT=8580

arch="" version="" upstream_tag="" bin_dir="" out_dir="out" icon="" variant="alias"
while [ $# -gt 0 ]; do
    case "$1" in
        --arch) arch="$2"; shift 2 ;;
        --version) version="$2"; shift 2 ;;
        --upstream-tag) upstream_tag="$2"; shift 2 ;;
        --bin-dir) bin_dir="$2"; shift 2 ;;
        --out-dir) out_dir="$2"; shift 2 ;;
        --icon) icon="$2"; shift 2 ;;
        --variant) variant="$2"; shift 2 ;;
        *) echo "unknown argument: $1" >&2; exit 2 ;;
    esac
done

die() { echo "build-spk: $*" >&2; exit 1; }

# INFO "arch" takes Synology CPU family names; the file name uses the common ones.
case "${arch}" in
    amd64|x86_64) syno_arch="x86_64"; file_arch="x86_64" ;;
    arm64|aarch64|armv8) syno_arch="armv8"; file_arch="aarch64" ;;
    *) die "--arch must be amd64 or arm64" ;;
esac
case "${variant}" in alias|portonly) ;; *) die "--variant must be alias or portonly" ;; esac
# DSM only accepts digits separated by . - _  (e.g. 2.1.5-0001).
echo "${version}" | grep -Eq '^[0-9]+(\.[0-9]+)*-[0-9]+$' || die "--version must look like 2.1.5-0001, got '${version}'"
[ -n "${upstream_tag}" ] || die "--upstream-tag is required"
for b in filebrowser ffmpeg ffprobe; do
    [ -f "${bin_dir}/${b}" ] || die "missing ${bin_dir}/${b}"
done
[ -z "${icon}" ] || [ -f "${icon}" ] || die "icon not found: ${icon}"

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
spk_src="${here}/spk"
mkdir -p "${out_dir}"
out_dir="$(cd "${out_dir}" && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

# --- icons -------------------------------------------------------------------
im=""
if command -v magick > /dev/null 2>&1; then
    im="magick"
elif command -v convert > /dev/null 2>&1 && convert -version 2> /dev/null | grep -q ImageMagick; then
    im="convert"
fi
make_icon() { # size destination
    if [ -z "${icon}" ]; then
        die "--icon is required"
    elif [ -n "${im}" ]; then
        "${im}" "${icon}" -resize "$1x$1" "$2"
    else
        cp "${icon}" "$2" # DSM scales it; only happens on machines without ImageMagick
    fi
}
[ -n "${im}" ] || echo "build-spk: ImageMagick not found, icons are copied unscaled" >&2

# --- package.tgz (becomes /var/packages/FileBrowserQuantum/target) -------------
target="${work}/target"
mkdir -p "${target}/bin" "${target}/ui/images"
cp -r "${spk_src}/target/." "${target}/"
cp "${bin_dir}/filebrowser" "${bin_dir}/ffmpeg" "${bin_dir}/ffprobe" "${target}/bin/"
for size in 16 24 32 48 64 72 256; do
    make_icon "${size}" "${target}/ui/images/icon_${size}.png"
done
[ "${variant}" = "alias" ] || rm -f "${target}/conf/nginx-dsm.conf"
find "${target}" -type d -exec chmod 755 {} +
find "${target}" -type f -exec chmod 644 {} +
chmod 755 "${target}/bin/"*

tar_opts=(--owner=0 --group=0 --numeric-owner --format=gnu)
tar "${tar_opts[@]}" -czf "${work}/package.tgz" -C "${target}" .
extract_kib="$(du -sk "${target}" | cut -f1)"

# --- outer archive --------------------------------------------------------------
stage="${work}/spk"
mkdir -p "${stage}/conf" "${stage}/scripts"
mv "${work}/package.tgz" "${stage}/package.tgz"
cp "${spk_src}/conf/privilege" "${stage}/conf/privilege"
cp "${spk_src}/conf/resource.${variant}.json" "${stage}/conf/resource"
cp "${spk_src}/scripts/"* "${stage}/scripts/"
# Tolerate CRLF checkouts: a "\r" after "#!/bin/sh" would break every script on the NAS.
sed -i 's/\r$//' "${stage}/scripts/"* "${stage}/conf/"*
make_icon 64 "${stage}/PACKAGE_ICON.PNG"
make_icon 256 "${stage}/PACKAGE_ICON_256.PNG"

nginx_lines=""
if [ "${variant}" = "alias" ]; then
    # The web-config worker does not reload nginx by itself.
    nginx_lines='instuninst_restart_services="nginx.service"
startstop_restart_services="nginx.service"'
fi
cat > "${stage}/INFO" <<EOF
package="${PKG_NAME}"
version="${version}"
arch="${syno_arch}"
os_min_ver="7.2-64561"
displayname="FileBrowser Quantum"
description="FileBrowser Quantum ${upstream_tag}: web file manager with bundled FFmpeg for image and video previews. Also writes video thumbnails for File Station, Synology Drive and Synology Photos."
description_cht="FileBrowser Quantum ${upstream_tag}：網頁檔案管理器，內建 FFmpeg 可產生圖片與影片預覽圖，並替 File Station、Synology Drive 與 Synology Photos 寫入影片縮圖。"
maintainer="FileBrowser Quantum (community Synology package)"
maintainer_url="https://github.com/gtsteffaniak/filebrowser"
thirdparty="yes"
dsmuidir="ui"
dsmappname="com.filebrowserquantum.app"
adminprotocol="http"
adminport="${PORT}"
adminurl="filebrowser/"
ctl_stop="yes"
silent_upgrade="yes"
extractsize="${extract_kib}"
create_time="$(date -u +%Y%m%d-%H:%M:%S)"
${nginx_lines}
EOF
sed -i '/^$/d' "${stage}/INFO"

chmod 644 "${stage}/INFO" "${stage}/package.tgz" "${stage}/conf/"* "${stage}/"*.PNG
chmod 755 "${stage}/scripts" "${stage}/conf" "${stage}/scripts/"*

suffix=""
[ "${variant}" = "alias" ] || suffix="-portonly"
spk="${out_dir}/${PKG_NAME}-${file_arch}-${version}${suffix}.spk"
# An .spk is a plain (uncompressed) tar with INFO first.
tar "${tar_opts[@]}" -cf "${spk}" -C "${stage}" INFO PACKAGE_ICON.PNG PACKAGE_ICON_256.PNG conf package.tgz scripts
echo "built ${spk} ($(du -h "${spk}" | cut -f1))"
