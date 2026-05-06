#!/usr/bin/env bash
# loopcop.sh — Runaway loop detector for Fordjent
# Monitors /status endpoint + Forgejo API for self-loop patterns
# Usage: ./scripts/loopcop.sh [interval_seconds] [duration_minutes]
set -euo pipefail

INTERVAL=${1:-30}     # seconds between checks
DURATION=${2:-60}     # total minutes to run
FORDJENT="http://127.0.0.1:8080"
FORGEJO="http://10.67.121.201:4230"
CREDS="duke:ollama"
ENCODED_CREDS=$(echo -n "$CREDS" | base64)

# Track state between checks
TMPDIR=$(mktemp -d)
STATE_FILE="$TMPDIR/loopcop_state.json"
COMMENT_COUNTS="$TMPDIR/comment_counts"
SESSION_TOKENS="$TMPDIR/session_tokens"

# Colors
RED='\033[0;31m'
YELLOW='\033[1;33m'
GREEN='\033[0;32m'
CYAN='\033[0;36m'
NC='\033[0m'

echo -e "${CYAN}═══ loopcop — Fordjent Runaway Loop Detector ═══${NC}"
echo -e "  Interval: ${INTERVAL}s  Duration: ${DURATION}m"
echo -e "  Fordjent: $FORDJENT  Forgejo: $FORGEJO"
echo ""

END_TIME=$(( $(date +%s) + DURATION * 60 ))
CHECK_NUM=0

while [ $(date +%s) -lt $END_TIME ]; do
    CHECK_NUM=$((CHECK_NUM + 1))
    NOW=$(date -Iseconds)
    ALERTS=0

    # 1. Check Fordjent /status
    STATUS=$(curl -s "$FORDJENT/status" 2>/dev/null || echo '{}')
    
    # Active sessions
    ACTIVE=$(echo "$STATUS" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('lifecycle',{}).get('active_sessions',0))" 2>/dev/null || echo "?")
    FAILED=$(echo "$STATUS" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('lifecycle',{}).get('failed_sessions',0))" 2>/dev/null || echo "?")
    
    # 2. Check per-session token growth
    echo "$STATUS" | python3 -c "
import sys, json, os

d = json.load(sys.stdin)
costs = d.get('costs', {})
records = costs.get('recent_records', [])

# Build session -> token map
current = {}
for r in records:
    sk = r.get('session_key', '')
    tok = r.get('tokens', 0)
    current[sk] = max(current.get(sk, 0), tok)

# Load previous state
state_file = '$STATE_FILE'
prev = {}
if os.path.exists(state_file):
    try:
        with open(state_file) as f:
            prev = json.load(f)
    except: pass

# Check for rapid growth (>50K tokens in one interval)
alerts = []
for sk, tok in current.items():
    prev_tok = prev.get(sk, 0)
    growth = tok - prev_tok
    if growth > 50000:
        alerts.append((sk, growth, tok))

# Save current state
with open(state_file, 'w') as f:
    json.dump(current, f)

if alerts:
    for sk, growth, total in alerts:
        print(f'ALERT:TOKEN_SPIKE session={sk} growth=+{growth:,} tokens total={total:,}')
" 2>/dev/null | while read -r line; do
        echo -e "${RED}[$NOW] $line${NC}"
        ALERTS=$((ALERTS + 1))
    done

    # 3. Check Forgejo for comment spam (bot posting >5 comments on same issue in window)
    for repo in $(curl -s -H "Authorization: Basic $ENCODED_CREDS" \
        "$FORGEJO/api/v1/repos/search?limit=20" 2>/dev/null | \
        python3 -c "import sys,json; [print(r['full_name']) for r in json.load(sys.stdin).get('data',[])]" 2>/dev/null); do
        
        # Get recent issues
        ISSUES=$(curl -s -H "Authorization: Basic $ENCODED_CREDS" \
            "$FORGEJO/api/v1/repos/$repo/issues?limit=20&state=all" 2>/dev/null || echo '[]')
        
        echo "$ISSUES" | python3 -c "
import sys, json, urllib.request, base64, os, time

issues = json.load(sys.stdin)
for issue in issues:
    num = issue.get('number', 0)
    # Count recent bot comments
    try:
        url = f'$FORGEJO/api/v1/repos/$repo/issues/{num}/comments?limit=50'
        req = urllib.request.Request(url, headers={'Authorization': 'Basic $ENCODED_CREDS'})
        resp = urllib.request.urlopen(req, timeout=5)
        comments = json.loads(resp.read())
        bot_comments = [c for c in comments if c.get('user',{}).get('login') == 'fordjent-bot']
        if len(bot_comments) > 10:
            print(f'ALERT:COMMENT_SPAM repo=$repo issue=#{num} bot_comments={len(bot_comments)}')
    except: pass
" 2>/dev/null | while read -r line; do
            echo -e "${YELLOW}[$NOW] $line${NC}"
            ALERTS=$((ALERTS + 1))
        done
    done

    # 4. Check for same session getting multiple rapid events
    # (detectable from Fordjent logs)
    docker logs fordjent --since "${INTERVAL}s" 2>&1 | grep -c "received event" | while read count; do
        if [ "$count" -gt 10 ]; then
            echo -e "${RED}[$NOW] ALERT:EVENT_FLOOD event_count=$count in ${INTERVAL}s${NC}"
        fi
    done

    # Print status line
    if [ "$ALERTS" -eq 0 ]; then
        echo -e "${GREEN}[$NOW] ✓ check#$CHECK_NUM active=$ACTIVE failed=$FAILED alerts=0${NC}"
    else
        echo -e "${YELLOW}[$NOW] ⚠ check#$CHECK_NUM active=$ACTIVE failed=$FAILED alerts=$ALERTS${NC}"
    fi

    sleep "$INTERVAL"
done

echo -e "${CYAN}═══ loopcop finished after $DURATION minutes ═══${NC}"
rm -rf "$TMPDIR"
