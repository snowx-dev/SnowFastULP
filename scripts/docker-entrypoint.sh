#!/busybox/sh
# Dispatch helper for the runtime image.
#
# Routes the container to sfu (default), sfs, or sfl based on the first
# positional argv element. Lets callers do:
#
#   docker run sfu:local input.txt -o ./out/        # sfu (default)
#   docker run sfu:local sfu input.txt -o ./out/    # explicit sfu
#   docker run sfu:local sfs ./library 'pattern'    # sfs
#   docker run sfu:local sfl ./logs/ -o ./ulp/      # sfl
#
# Distroless `:debug-nonroot` ships /busybox/sh, so the shebang above is
# stable even though there's no /bin/sh symlink in some image revisions.
set -e

case "$1" in
    sfu|sfs|sfl)
        bin="/usr/local/bin/$1"
        shift
        exec "$bin" "$@"
        ;;
    *)
        exec /usr/local/bin/sfu "$@"
        ;;
esac
