#!/bin/bash
# Probe on containerd's filesystem before choosing overlayfs. An operator may
# explicitly select native; no GPL FUSE helper is shipped or invoked.
select_snapshotter() {
    case "${SNAPSHOTTER:-}" in
        native|overlayfs) return ;;
        "") ;;
        *) echo "unsupported snapshotter: ${SNAPSHOTTER} (want overlayfs or native)" >&2; return 1 ;;
    esac
    probe=/var/lib/rancher/k3s/agent/.overlay-probe
    mkdir -p "$probe/lower" "$probe/upper" "$probe/work" "$probe/merged"
    if mount -t overlay overlay -o "lowerdir=$probe/lower,upperdir=$probe/upper,workdir=$probe/work" "$probe/merged" 2>/dev/null; then
        umount "$probe/merged" || return 1
        SNAPSHOTTER=overlayfs
    else
        SNAPSHOTTER=native
    fi
    rm -rf "$probe"
}
