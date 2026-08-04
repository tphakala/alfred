#!/usr/bin/env bash
set -euo pipefail
cat <<'EOF'
{"type":"system","subtype":"init","session_id":"sess-fake","model":"claude-test"}
{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}}
{"type":"result","is_error":false,"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15},"total_cost_usd":0.001}
EOF
exit 0
