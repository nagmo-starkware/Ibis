#!/usr/bin/env bash
# Fetch a Starknet contract ABI via JSON-RPC starknet_getClassAt
#
# Usage: fetch_abi.sh <contract_address> [rpc_url]
#
# Default RPC: https://starknet-rpc.publicnode.com
# For sepolia: https://starknet-sepolia-rpc.publicnode.com
#
# Output: raw ABI JSON array to stdout

set -euo pipefail

CONTRACT_ADDRESS="${1:?Usage: fetch_abi.sh <contract_address> [rpc_url]}"
RPC_URL="${2:-https://starknet-rpc.publicnode.com}"

# Fetch class at latest block
RESPONSE=$(curl -s -X POST "$RPC_URL" \
  -H "Content-Type: application/json" \
  -d "{
    \"jsonrpc\": \"2.0\",
    \"method\": \"starknet_getClassAt\",
    \"params\": {
      \"block_id\": \"latest\",
      \"contract_address\": \"$CONTRACT_ADDRESS\"
    },
    \"id\": 1
  }")

# Check for JSON-RPC error
ERROR=$(echo "$RESPONSE" | python3 -c "
import sys, json
data = json.load(sys.stdin)
if 'error' in data:
    print(json.dumps(data['error']))
else:
    print('')
" 2>/dev/null || echo "PARSE_ERROR")

if [ -n "$ERROR" ] && [ "$ERROR" != "" ]; then
  echo "RPC Error: $ERROR" >&2
  exit 1
fi

# Extract ABI - handles both Sierra (string) and deprecated (object) formats
echo "$RESPONSE" | python3 -c "
import sys, json

data = json.load(sys.stdin)
result = data.get('result', {})
abi = result.get('abi')

if abi is None:
    print('Error: No ABI field in class output', file=sys.stderr)
    sys.exit(1)

# Sierra contracts (modern): ABI is a JSON string
if isinstance(abi, str):
    parsed = json.loads(abi)
    print(json.dumps(parsed, indent=2))
# Deprecated contracts (Cairo 0): ABI is a JSON array/object
elif isinstance(abi, list):
    print(json.dumps(abi, indent=2))
else:
    print(json.dumps(abi, indent=2))
"
