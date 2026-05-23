#!/bin/bash
set -euo pipefail

FORGEJO_URL="${FORGEJO_URL:-http://localhost:3000}"
ADMIN_TOKEN="${ADMIN_TOKEN:-}"

if [ -z "$ADMIN_TOKEN" ]; then
    echo "Error: ADMIN_TOKEN not set"
    exit 1
fi

PASSWORD="${ROLE_USER_PASSWORD:-Fordjent123!}"

echo "Creating role users..."

# Create users via admin API
for user in djent-pm djent-dev djent-qa; do
    email="${user}@fordjent.local"
    
    # Check if exists
    status=$(curl -s -o /dev/null -w "%{http_code}" \
        "$FORGEJO_URL/api/v1/users/$user" \
        -H "Authorization: token $ADMIN_TOKEN")
    
    if [ "$status" = "200" ]; then
        echo "  $user already exists"
    else
        resp=$(curl -sf -X POST "$FORGEJO_URL/api/v1/admin/users" \
            -H "Authorization: token $ADMIN_TOKEN" \
            -H "Content-Type: application/json" \
            -d "{\"username\":\"$user\",\"email\":\"$email\",\"password\":\"$PASSWORD\",\"must_change_password\":false,\"login_name\":\"$user\"}")
        
        if [ $? -eq 0 ]; then
            echo "  Created $user"
        else
            echo "  Failed to create $user: $resp"
        fi
    fi
done

echo ""
echo "Creating tokens..."

# Create tokens for each user (must auth as the user)
for user in djent-pm djent-dev djent-qa; do
    resp=$(curl -sf -X POST "$FORGEJO_URL/api/v1/users/$user/tokens" \
        -u "${user}:${PASSWORD}" \
        -H "Content-Type: application/json" \
        -d '{"name":"fordjent-role","scopes":["read:repository","write:repository","read:issue","write:issue","read:user","write:user","write:activitypub","read:activitypub","write:notification","read:notification","read:misc","write:package","read:package"]}')
    
    if [ $? -eq 0 ]; then
        sha=$(echo "$resp" | python3 -c "import sys,json; print(json.load(sys.stdin)['sha1'])")
        echo "  $user token: $sha"
    else
        echo "  Failed to create token for $user: $resp"
    fi
done

echo ""
echo "# Add this to fordjent.yaml under forgejo:"
echo "role_tokens:"
echo "  pm: \"TOKEN_FROM_ABOVE\""
echo "  implementer: \"TOKEN_FROM_ABOVE\""
echo "  devops: \"TOKEN_FROM_ABOVE\""
echo "  reviewer: \"TOKEN_FROM_ABOVE\""
echo "  tester: \"TOKEN_FROM_ABOVE\""
echo "role_users:"
echo "  pm: \"djent-pm\""
echo "  implementer: \"djent-dev\""
echo "  devops: \"djent-dev\""
echo "  reviewer: \"djent-qa\""
echo "  tester: \"djent-qa\""
