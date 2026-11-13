#!/usr/bin/env bash
#
# Build and check the changelog from the entry files in .changelog/.
#
# Usage:
#   scripts/changelog.sh preview             Print the entries that are not in a release yet.
#   scripts/changelog.sh notes               Print the release notes for the commit that is checked out.
#   scripts/changelog.sh check               Check the format of all entry files.

set -euo pipefail

# Run from the root of the repository, so that the paths below are correct from any directory.
cd "$(git rev-parse --show-toplevel)"

readonly ENTRIES_DIR=".changelog"
readonly TEMPLATES_DIR=".changelog-templates"
readonly NOTE_TYPES="breaking-change upgrade-note new-resource new-data-source feature enhancement bug note"

# Print the last release tag before the commit that is checked out, or nothing if there is none.
# A tag on the checked-out commit is not "before": the release workflow runs on the new tag.
last_tag() {
  local at_head
  at_head="$(git tag --points-at HEAD)"
  # versionsort.suffix makes a pre-release tag such as v1.0.0-rc1 sort before v1.0.0.
  git -c versionsort.suffix=- tag --merged HEAD --list 'v[0-9]*' --sort=-v:refname |
    grep -vxF -e "${at_head:-/no-tag/}" | head -n 1 || true
}

# Print the entries added since the last release tag.
build() {
  local last
  last="$(last_tag)"
  # The tool needs the entries directory at the last release. A tag from before
  # this directory existed has none. Then the tool must use all entries.
  if [[ -z "${last}" ]] || ! git cat-file -e "${last}:${ENTRIES_DIR}" 2>/dev/null; then
    last="-"
  fi
  go run github.com/hashicorp/go-changelog/cmd/changelog-build \
    -git-dir "$(pwd)" \
    -last-release "${last}" \
    -this-release "$(git rev-parse HEAD)" \
    -entries-dir "${ENTRIES_DIR}" \
    -note-template "${TEMPLATES_DIR}/note.tmpl" \
    -changelog-template "${TEMPLATES_DIR}/changelog.tmpl"
}

preview() {
  if [[ -n "$(git status --porcelain -- "${ENTRIES_DIR}")" ]]; then
    echo "warning: the tool reads committed entries only. Commit ${ENTRIES_DIR}/ first." >&2
  fi
  build
}

notes() {
  local body
  body="$(build)"
  if [[ -z "${body}" ]]; then
    echo "error: there are no new entries since the last release." >&2
    exit 1
  fi
  printf '%s\n' "${body}"
}

check() {
  local status=0 file name pr
  shopt -s nullglob dotglob
  local files=("${ENTRIES_DIR}"/*)
  if [[ ${#files[@]} -eq 0 ]]; then
    echo "error: ${ENTRIES_DIR}/ has no entry files." >&2
    return 1
  fi
  # The build reads every file in the directory, so check all of them.
  for file in "${files[@]}"; do
    name="$(basename "${file}")"
    if [[ ! "${name}" =~ ^([0-9]+)\.txt$ ]]; then
      echo "${file}: the file name must be the number of the main pull request and .txt, such as 154.txt." >&2
      status=1
      continue
    fi
    pr="${BASH_REMATCH[1]}"
    if ! awk -v types="${NOTE_TYPES}" -v file="${file}" -v pr="${pr}" '
      BEGIN { n = split(types, t, " "); for (i = 1; i <= n; i++) valid[t[i]] = 1 }
      /^```release-note:/ {
        if (inblock) { fail("block is not closed before line " NR) }
        type = substr($0, 17)
        if (!(type in valid)) { fail("line " NR ": unknown note type \"" type "\"") }
        inblock = 1; first = ""; last = ""; blocks++; next
      }
      /^```[[:space:]]*$/ {
        if (!inblock) { fail("line " NR ": closing fence without an open block") }
        else if (first == "") { fail("line " NR ": the block has no text") }
        else { check_marker(NR) }
        inblock = 0; next
      }
      inblock {
        if ($0 ~ /[^[:space:]]/) { if (first == "") first = $0; last = $0 }
        next
      }
      /[^[:space:]]/ { fail("line " NR ": text outside a block") }
      END {
        if (inblock) { fail("the last block is not closed") }
        if (!blocks) { fail("the file has no release-note block") }
        exit bad
      }
      # The entry ends with a marker such as (#154) or (#154, #158). If the entry
      # has a list, the marker can end the first line instead.
      function check_marker(line,    text) {
        marker = "\\(#[0-9]+(, #[0-9]+)*\\)$"
        if (last ~ marker) text = last
        else if (first ~ marker) text = first
        else { fail("line " line ": the entry must end with a marker such as (#" pr ")"); return }
        # Keep the part after the "(#" of the last marker, then compare its first number.
        sub(/^.*\(#/, "", text)
        sub(/[^0-9].*$/, "", text)
        if (text != pr) {
          fail("line " line ": the first number of the marker must be " pr ", the number in the file name")
        }
      }
      function fail(msg) { print file ": " msg > "/dev/stderr"; bad = 1 }
    ' "${file}"; then
      status=1
    fi
  done
  return "${status}"
}

main() {
  case "${1:-}" in
    preview) preview ;;
    notes) notes ;;
    check) check ;;
    *)
      echo "usage: $0 preview | notes | check" >&2
      exit 2
      ;;
  esac
}

main "$@"
