#!/usr/bin/env bash
# fake-copilot-mcp.sh records argv so tests can assert which flags were
# passed to the copilot subprocess. If the additional MCP config flag is
# present, it copies that file into ${MCP_CAPTURE_FILE} so the test can
# inspect what the runner wrote.
set -euo pipefail

if [[ -n "${ARGV_CAPTURE_FILE:-}" ]]; then
  : > "${ARGV_CAPTURE_FILE}"
  for arg in "$@"; do
    printf '%s\n' "${arg}" >> "${ARGV_CAPTURE_FILE}"
  done
fi

if [[ -n "${MCP_CAPTURE_FILE:-}" ]]; then
  found_path=""
  prev=""
  for arg in "$@"; do
    if [[ "${prev}" == "--additional-mcp-config" ]]; then
      found_path="${arg}"
      break
    fi
    prev="${arg}"
  done
  if [[ -n "${found_path}" && -f "${found_path}" ]]; then
    cp "${found_path}" "${MCP_CAPTURE_FILE}"
  fi
fi

cat <<'EOF'
{"type":"session_start","session_id":"s-1","model":"copilot-test"}
{"type":"complete","is_error":false,"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}
EOF
