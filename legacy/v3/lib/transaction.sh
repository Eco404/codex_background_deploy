#!/usr/bin/env bash
# Write-ahead recovery. Retain all evidence if any restoration fails.
cb_recover() {
    local txn="$CB_STATE/transaction" stage='' upgrade=0 previous='' p key node config record
    [ -d "$txn" ] || { cb_info '没有待恢复事务。'; return 0; }
    printf '回滚未完成安装；恢复旧入口和配置。可能停止本次启动的后台。\n'
    cb_confirm || return 0
    cb_lock || return
    IFS= read -r stage < "$txn/stage" || return
    [[ $stage = "$CB_ROOT".stage.* ]] || { cb_error '无效事务路径'; return 1; }
    [ ! -f "$txn/upgrade" ] || read -r upgrade < "$txn/upgrade"
    if [ -f "$txn/previous" ]; then
        read -r previous < "$txn/previous"
        [ "$previous" = "${CB_ROOT}.previous" ] || return 1
    fi
    config="$CB_ROOT/config.sh"
    [ -f "$config" ] || config="$stage/config.sh"
    [ -f "$config" ] || config="$previous/config.sh"
    if [ -f "$config" ]; then
        local expected_root=$CB_ROOT expected_state=$CB_STATE
        . "$config" || return
        [ "$CB_ROOT" = "$expected_root" ] && [ "$CB_STATE" = "$expected_state" ] || return 1
    fi
    . "$SOURCE_ROOT/payload/bridge/resolve-node.sh" || return
    node=$(cb_resolve_node) || return
    for record in "$txn/new-settings/"*.path; do
        [ -f "$record" ] || continue
        IFS= read -r p < "$record"; key=${record##*/}; key=${key%.path}
        if [ -f "$CB_STATE/settings/$key.json" ]; then
            "$node" "$SOURCE_ROOT/payload/settings.mjs" restore "$p" "$CB_STATE/settings/$key.json" || return
        fi
        rm -f -- "$CB_STATE/settings/$key.path" || return
    done
    if [ -f "$txn/selecting" ] && [ -f "$CB_ROOT/config.sh" ]; then
        "$CB_ROOT/payload/control.sh" stop || return
    fi
    if [ -f "$txn/linger-new" ]; then
        sudo loginctl disable-linger "$(id -un)" || return
        rm -f -- "$CB_STATE/linger-owned" || return
    fi
    # Preserve external files on recovery if they no longer match our last write.
    for record in "$txn/adapters/"*.path; do
        [ -f "$record" ] || continue
        IFS= read -r p < "$record"
        key=${record##*/}; key=${key%.path}
        if [ -f "$CB_STATE/files/$key" ] && [ -f "$p" ]; then
            local recorded_hash actual_hash
            recorded_hash=$(sed -n '2p' "$CB_STATE/files/$key")
            actual_hash=$(cb_hash "$p") || return
            [ "$recorded_hash" = "$actual_hash" ] || { cb_error "恢复冲突，保留：$p"; return 1; }
        fi
        if [ -f "$txn/adapters/$key.original" ]; then
            cb_atomic_copy "$txn/adapters/$key.original" "$p" "$(stat -c %a "$txn/adapters/$key.original")" || return
        elif [ -f "$txn/selecting" ]; then
            if [ "$p" = "$UNIT" ] && [ "$CB_BACKEND" = systemd ]; then systemctl --user disable codex-background.service || return; fi
            rm -f -- "$p" || return
        fi
    done
    if [ -n "$previous" ] && [ -d "$previous" ]; then
        # A failed new payload contains no user data, only installer-owned runtime.
        if [ -d "$CB_ROOT" ]; then
            [ -f "$CB_ROOT/managed-v3" ] || return 1
            rm -rf -- "$CB_ROOT" || return
        fi
        mv -- "$previous" "$CB_ROOT" || return
    elif [ "$upgrade" = 0 ] && [ -f "$txn/selecting" ] && [ -d "$CB_ROOT" ]; then
        [ -f "$CB_ROOT/managed-v3" ] || return 1
        rm -rf -- "$CB_ROOT" || return
    fi
    rm -rf -- "$CB_STATE/files" || return
    if [ -d "$txn/files" ]; then cp -a -- "$txn/files" "$CB_STATE/files" || return; fi
    if [ "${CB_BACKEND:-}" = systemd ]; then systemctl --user daemon-reload || return; fi
    if [ "$upgrade" = 1 ] && [ -f "$CB_ROOT/config.sh" ]; then
        "$CB_ROOT/payload/control.sh" start || return
    fi
    rm -rf -- "$stage" "$txn" || return
    if [ "$upgrade" = 0 ]; then rm -rf -- "$CB_STATE" || return; fi
    cb_info '未完成安装已回滚。'
}
