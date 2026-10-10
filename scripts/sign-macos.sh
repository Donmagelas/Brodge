#!/bin/sh
set -eu
umask 077

fail() { printf '%s\n' "$*" >&2; exit 1; }
[ "$(uname -s)" = Darwin ] || fail 'Brodge app signing requires macOS.'
[ "$#" -eq 1 ] || fail 'Usage: sign-macos.sh --setup | /path/to/Brodge.app'
target=$1

# A release identity can be supplied explicitly. Never fall back to ad hoc.
if [ "$target" != --setup ] && [ -n "${BRODGE_CODESIGN_IDENTITY:-}" ]; then
    [ "$BRODGE_CODESIGN_IDENTITY" != - ] || fail 'Ad hoc signing cannot preserve Brodge permissions.'
    codesign --force --timestamp=none --sign "$BRODGE_CODESIGN_IDENTITY" "$target"
    exec codesign --verify --deep --strict "$target"
fi

signing_dir=${BRODGE_SIGNING_DIR:-"$HOME/Library/Application Support/Brodge/signing"}
keychain="$signing_dir/identity.keychain-db"
password_file="$signing_dir/keychain-password"
certificate="$signing_dir/certificate.pem"
identity_file="$signing_dir/identity.sha1"

# codesign also requires this dedicated keychain in the user search list.
# Preserve every existing entry and the user's default keychain.
make_searchable() {
    entries=$(security list-keychains -d user)
    set --
    found=false
    while IFS= read -r entry; do
        case "$entry" in
            *\"*\") entry=${entry#*\"}; entry=${entry%\"} ;;
            *) fail 'Cannot read the existing keychain search list.' ;;
        esac
        [ "$entry" != "$keychain" ] || found=true
        set -- "$@" "$entry"
    done <<EOF
$entries
EOF
    if [ "$found" = false ]; then
        security list-keychains -d user -s "$@" "$keychain"
    fi
}

if [ "$target" = --setup ]; then
    mkdir -p "$signing_dir"
    [ ! -L "$signing_dir" ] || fail 'Signing directory must not be a symlink.'
    chmod 700 "$signing_dir"
    if [ ! -e "$keychain" ] && [ ! -e "$password_file" ] && [ ! -e "$certificate" ] && [ ! -e "$identity_file" ]; then
        work=$(mktemp -d "$signing_dir/setup.XXXXXX")
        trap 'rm -rf "$work"' EXIT HUP INT TERM
        openssl rand -hex 32 > "$password_file"
        IFS= read -r password < "$password_file"
        security create-keychain -p "$password" "$keychain"
        security set-keychain-settings -lut 21600 "$keychain"
        cat > "$work/certificate.cnf" <<'EOF'
[req]
prompt=no
distinguished_name=subject
x509_extensions=extensions
[subject]
CN=Brodge Local Development
[extensions]
basicConstraints=critical,CA:false
keyUsage=critical,digitalSignature
extendedKeyUsage=critical,codeSigning
subjectKeyIdentifier=hash
EOF
        openssl req -new -newkey rsa:3072 -nodes -x509 -days 3650 -config "$work/certificate.cnf" -keyout "$work/private.pem" -out "$certificate" 2>/dev/null
        openssl pkcs12 -export -inkey "$work/private.pem" -in "$certificate" -out "$work/identity.p12" -passout "file:$password_file"
        security import "$work/identity.p12" -k "$keychain" -P "$password" -T /usr/bin/codesign >/dev/null
        security set-key-partition-list -S apple-tool:,apple:,codesign: -s -k "$password" "$keychain" >/dev/null
        openssl x509 -in "$certificate" -noout -fingerprint -sha1 | sed 's/.*=//; s/://g' > "$identity_file"
    fi
fi

for file in "$keychain" "$password_file" "$certificate" "$identity_file"; do
    [ -f "$file" ] && [ ! -L "$file" ] || fail 'Persistent signing identity is missing or incomplete. Run scripts/sign-macos.sh --setup once; restore an existing identity from backup instead of replacing it.'
done
IFS= read -r identity < "$identity_file"
fingerprint=$(openssl x509 -in "$certificate" -noout -fingerprint -sha1 | sed 's/.*=//; s/://g')
[ "$identity" = "$fingerprint" ] && [ "${#identity}" -eq 40 ] || fail 'Signing certificate identity changed; refusing to replace the application.'
IFS= read -r password < "$password_file"
security unlock-keychain -p "$password" "$keychain"
make_searchable

if [ "$target" = --setup ]; then
    # User-domain trust is restricted to this certificate's code-signing policy.
    # This may ask for macOS confirmation once. No system/TLS trust is installed.
    security add-trusted-cert -r trustRoot -p codeSign -k "$keychain" "$certificate"
    printf '%s\n' 'Brodge persistent local signing identity is ready.'
else
    codesign --force --timestamp=none --keychain "$keychain" --sign "$identity" "$target"
    codesign --verify --deep --strict "$target"
fi
