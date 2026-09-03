#!/bin/bash
#
# install.sh 的校验和逻辑测试。
#
# install.sh 是"curl | sh"这条链路上唯一一道检查，改坏了没有任何编译器或 go test 会拦，
# 而它出问题的表现是"装上了一个不对的二进制"——安静且致命。所以这里把它当被测程序跑：
# 用 PATH 前置的假 curl 把 release 的响应完全接管，喂各种 SHA256SUMS 组合，断言它该装的
# 时候装、该拒的时候拒。
#
# 用法：bash tests/install/install_sh_test.sh [被测的 install.sh 路径]

set -u

SCRIPT=${1:-"$(dirname "$0")/../../install.sh"}
if [ ! -f "$SCRIPT" ]; then
    echo "找不到被测脚本: $SCRIPT" >&2
    exit 1
fi

TMP=$(mktemp -d) || exit 1
# 下面这些目录要塞进 PATH，而 PATH 用冒号分隔——带盘符的 "C:/..." 会被切成 "C" 和
# "/..."，桩就永远找不到了。Git Bash 下必须先转成 POSIX 形式。
if command -v cygpath >/dev/null 2>&1; then TMP=$(cygpath -u "$TMP"); fi
trap 'rm -rf "$TMP"' EXIT

fails=0
note_fail() { echo "FAIL $1"; fails=$((fails + 1)); }

# ---------------------------------------------------------------- 下载桩

STUB="$TMP/stub"
mkdir -p "$STUB"

# 假 curl：把 URL 的最后一段映射到 $STUB_SERVE_DIR 下的同名文件，没有就报 404。
# 只实现 install.sh 实际用到的 "--output <path> ... <url>" 这一种调用形式。
cat > "$STUB/curl" <<'EOF'
#!/bin/sh
out=''
url=''
while [ $# -gt 0 ]; do
    case "$1" in
        --output) out=$2; shift 2 ;;
        -*) shift ;;
        *) url=$1; shift ;;
    esac
done
src="${STUB_SERVE_DIR}/${url##*/}"
[ -f "$src" ] || { echo "stub-curl: 404 $url" >&2; exit 22; }
cat "$src" > "$out"
EOF

# 假 wget：CI runner 上真有 wget，不挡住的话下载失败的用例会真去连 github.com。
cat > "$STUB/wget" <<'EOF'
#!/bin/sh
echo "stub-wget: refused" >&2
exit 1
EOF

chmod +x "$STUB/curl" "$STUB/wget"

# PATH 收窄到"桩 + 基础工具"。除了让桩排在最前，更重要的是把 powershell.exe 排除掉：
# install.sh 在 Windows 上会拿它当第三条下载后路，一次真实超时就是几十秒。
TESTPATH="$STUB:/usr/bin:/bin"

# ---------------------------------------------------------------- 夹具

case "$(uname -s)" in
    Linux*)  os=linux;   ext='' ;;
    Darwin*) os=darwin;  ext='' ;;
    *)       os=windows; ext='.exe' ;;
esac
case "$(uname -m)" in
    x86_64|amd64)   arch=amd64 ;;
    arm64|aarch64)  arch=arm64 ;;
    *) echo "skip: 不支持的架构 $(uname -m)"; exit 0 ;;
esac
ASSET="remote-assist-cli-${os}-${arch}${ext}"
PAYLOAD='fake binary payload'

# 期望值不能只认 sha256sum——macOS 上根本没有这个命令，而 macOS 正是 install.sh 明确支持
# 的平台之一；写死会让这套测试在开发者本机直接算出空串，然后一路"通过"。
reference_sha256() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum | cut -d ' ' -f 1
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 | cut -d ' ' -f 1
    elif command -v openssl >/dev/null 2>&1; then
        openssl dgst -sha256 | sed 's/.*= *//'
    else
        return 1
    fi
}
GOOD=$(printf '%s\n' "$PAYLOAD" | reference_sha256)
if [ ${#GOOD} -ne 64 ] || [ -n "$(printf '%s' "$GOOD" | tr -d '0-9a-f')" ]; then
    echo "skip: 本机没有可用的 SHA-256 工具（拿到的期望值是 '$GOOD'）"
    exit 0
fi

# 有 dash 就用 dash 跑 install.sh。它是 Debian/Ubuntu 上的 /bin/sh，也就是绝大多数
# "curl | sh" 用户实际执行这个脚本的解释器；它对 POSIX 之外的写法比 bash 严格得多，
# 用 bash 跑的话 local/[[ ]]/数组这类 bashism 会一路蒙混过关，到用户机器上才炸。
SH_UNDER_TEST=${INSTALL_SH_SHELL:-$(command -v dash 2>/dev/null)}
[ -n "$SH_UNDER_TEST" ] || SH_UNDER_TEST=/bin/sh

# 执行的是去掉 CR 的副本，而不是工作区里那份。Windows 上 core.autocrlf 会把签出的
# install.sh 变成 CRLF，dash 会被行尾的 \r 噎死（"set: Illegal option -"）——那是本地
# 签出的产物，不是脚本内容的问题。用户拿到的是 raw.githubusercontent 上的 blob，这里
# 复现的就是那一份。行尾本身单独用下面的 i/lf 断言来守。
RUN_COPY="$TMP/install_under_test.sh"
tr -d '\r' < "$SCRIPT" > "$RUN_COPY"

echo "被测资产: $ASSET"
echo "解释器:   $SH_UNDER_TEST"

# 仓库里存的必须是 LF。install.sh 是 "curl ... | sh" 直接执行的，存成 CRLF 的话
# Linux/macOS 用户会收到一串 \r 引起的莫名其妙报错。.gitattributes 已经钉死，这条
# 是它的守卫——只在被测脚本确实是仓库里那份时才检查。
if command -v git >/dev/null 2>&1 && [ "$(basename "$SCRIPT")" = install.sh ]; then
    eol=$(git -C "$(dirname "$SCRIPT")" ls-files --eol -- install.sh 2>/dev/null)
    case "$eol" in
        '') ;; # 不在 git 仓库里（比如单独拷出来测），跳过
        i/lf*) echo "ok   install.sh 在仓库里存的是 LF" ;;
        *) note_fail "install.sh 在仓库里不是 LF: $eol" ;;
    esac
fi

DEST="$TMP/dest"

# serve <目录> <SHA256SUMS 内容；SKIP 表示不提供该文件>
serve() {
    rm -rf "$1"; mkdir -p "$1"
    printf '%s\n' "$PAYLOAD" > "$1/$ASSET"
    [ "$2" = SKIP ] || printf '%s' "$2" > "$1/SHA256SUMS"
}

# run <目录> [额外的环境变量赋值...] -> 设置 rc / out
run() {
    d=$1; shift
    rm -rf "$DEST"; mkdir -p "$DEST"
    out=$(env PATH="$TESTPATH" STUB_SERVE_DIR="$d" REMOTE_INSTALL_DIR="$DEST" "$@" \
        "$SH_UNDER_TEST" "$RUN_COPY" 2>&1)
    rc=$?
}

installed() { [ -f "$DEST/remote" ] || [ -f "$DEST/remote.exe" ]; }

# expect <用例名> <期望成功?0/1> <期望输出里含有的串>
expect() {
    if [ "$rc" -eq 0 ]; then ok=0; else ok=1; fi
    if [ "$ok" -ne "$2" ]; then
        note_fail "$1: rc=$rc 与预期不符"; printf '%s\n' "$out"; return
    fi
    case "$out" in
        *"$3"*) echo "ok   $1" ;;
        *) note_fail "$1: 输出里没有 '$3'"; printf '%s\n' "$out" ;;
    esac
}

# ---------------------------------------------------------------- 用例

S="$TMP/serve"

serve "$S" "$GOOD  $ASSET
"
run "$S"
expect "校验和匹配时正常安装" 0 "Checksum verified."
installed || note_fail "校验通过却没把文件装上"

serve "$S" "0000000000000000000000000000000000000000000000000000000000000000  $ASSET
"
run "$S"
expect "校验和不匹配时中止" 1 "checksum mismatch"
# 最要紧的一条：校验失败绝不能留下一个可执行的坏文件
! installed || note_fail "校验失败却还是把文件装上了"
if ls -a "$DEST" 2>/dev/null | grep -q '\.tmp\.\|\.sums'; then
    note_fail "校验失败后残留了临时文件"
else
    echo "ok   校验失败后不留临时文件"
fi

# 取不到清单必须当失败处理。若改成"拿不到就跳过"，任何能拦掉一个请求的人
# 就能让校验形同虚设。
serve "$S" SKIP
run "$S"
expect "取不到 SHA256SUMS 时中止" 1 "cannot download the checksum list"

serve "$S" "$GOOD  some-other-asset
"
run "$S"
expect "资产未列入清单时中止" 1 "is not listed in SHA256SUMS"

# coreutils 的二进制模式会在文件名前加 '*'，两种格式都得认
serve "$S" "$GOOD *$ASSET
"
run "$S"
expect "认得二进制模式的 * 前缀" 0 "Checksum verified."

serve "$S" SKIP
run "$S" REMOTE_INSTALL_SKIP_CHECKSUM=1
expect "逃生舱可跳过校验" 0 "skipping checksum verification"

# ------------------------------------------- compute_sha256 的三条工具分支
#
# macOS 自带的是 perl 版 shasum 而没有 sha256sum，某些精简容器里两个都没有只剩
# openssl。本机只会走到其中一条，所以这里按分支逐个逼出来：给每条分支准备一个
# 只含该工具的 PATH 目录。函数直接从 install.sh 里抠出来，保证测的是真代码。

FN="$TMP/compute_sha256.sh"
sed -n '/^compute_sha256() {/,/^}/p' "$RUN_COPY" > "$FN"
if [ ! -s "$FN" ]; then
    note_fail "没能从 install.sh 里提取 compute_sha256（函数被改名了？）"
else
    probe="$TMP/probe.bin"
    printf '%s\n' "$PAYLOAD" > "$probe"

    wrap() { # <目录> <命令名> <真实路径>
        printf '#!/bin/sh\nexec "%s" "$@"\n' "$3" > "$1/$2"
        chmod +x "$1/$2"
    }
    compute_with() { env PATH="$1" /bin/sh -c ". '$FN'; compute_sha256 '$probe'" 2>&1; }

    for tool in sha256sum shasum openssl; do
        bin=$(command -v "$tool" 2>/dev/null) || bin=''
        if [ -z "$bin" ]; then echo "skip 本机没有 $tool"; continue; fi
        d="$TMP/only-$tool"; rm -rf "$d"; mkdir -p "$d"
        # 这些桩是 shebang 脚本，得由 shell 来 exec；PATH 里必须能找到 sh 自己
        wrap "$d" sh "$(command -v sh)"
        wrap "$d" cut "$(command -v cut)"
        wrap "$d" sed "$(command -v sed)"
        wrap "$d" "$tool" "$bin"
        got=$(compute_with "$d")
        if [ "$got" = "$GOOD" ]; then
            echo "ok   只有 $tool 时也算得出正确的 SHA-256"
        else
            note_fail "只有 $tool 时算错了: got='$got' want='$GOOD'"
        fi
    done

    # 路径里带空格和反斜杠时不能算错。GNU coreutils 只要在文件名里看到反斜杠或换行，
    # 就会在整行最前面补一个转义标记 '\'，于是 "cut 取第一段" 取到的是 '\<hash>'——
    # 传文件名的写法会踩中，走 stdin 的写法不会。这不是纸面问题：install.sh 明确支持
    # Windows，而那儿 REMOTE_INSTALL_DIR=C:\foo 是最自然的写法。
    #
    # 两个平台的构造方式不同：类 Unix 上反斜杠只是文件名里的普通字符，直接建就行；
    # Windows(msys) 上建不出这种文件名，但反斜杠可以当路径分隔符，照样能凑出一个
    # 「含反斜杠且指得到真实文件」的路径。
    awkward=''
    # 子 shell 包一层：重定向失败的报错是 shell 自己发的，命令上挂 2>/dev/null 拦不住
    if ( printf '%s\n' "$PAYLOAD" > "$TMP/we ird\\path.bin" ) 2>/dev/null \
        && [ -f "$TMP/we ird\\path.bin" ]; then
        awkward="$TMP/we ird\\path.bin"
    else
        mkdir -p "$TMP/sub"
        printf '%s\n' "$PAYLOAD" > "$TMP/sub/we ird.bin"
        [ -f "$TMP/sub\\we ird.bin" ] && awkward="$TMP/sub\\we ird.bin"
    fi
    if [ -n "$awkward" ]; then
        got=$(/bin/sh -c ". '$FN'; compute_sha256 \"\$1\"" _ "$awkward" 2>&1)
        if [ "$got" = "$GOOD" ]; then
            echo "ok   路径含空格与反斜杠时仍算得对"
        else
            note_fail "路径含空格与反斜杠时算错了: got='$got' want='$GOOD'"
        fi
    else
        echo "skip 本机凑不出含反斜杠的可用路径"
    fi

    d="$TMP/no-hash-tool"; rm -rf "$d"; mkdir -p "$d"
    wrap "$d" sh "$(command -v sh)"
    wrap "$d" cut "$(command -v cut)"
    wrap "$d" sed "$(command -v sed)"
    if compute_with "$d" >/dev/null 2>&1; then
        note_fail "一个哈希工具都没有却返回了成功（会把空串当成校验和）"
    else
        echo "ok   没有任何哈希工具时返回失败"
    fi
fi

echo "----"
if [ "$fails" -eq 0 ]; then
    echo "install.sh: 全部通过"
else
    echo "install.sh: $fails 项失败"
    exit 1
fi
