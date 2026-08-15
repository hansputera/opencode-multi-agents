#!/bin/bash

# Test script for OpenAI-compatible API Gateway

set -e

GATEWAY_URL="${GATEWAY_URL:-http://localhost:8082}"
API_KEY="${UPSTREAM_API_KEY:-your-api-key-here}"

echo "==================================="
echo "Testing OpenAI API Gateway"
echo "Gateway URL: $GATEWAY_URL"
echo "==================================="
echo ""

# Test 1: Health Check
echo "Test 1: Health Check"
echo "-----------------------------------"
curl -s "$GATEWAY_URL/health" | jq '.'
echo ""
echo ""

# Test 2: Pool Statistics
echo "Test 2: Pool Statistics"
echo "-----------------------------------"
curl -s "$GATEWAY_URL/stats" | jq '.'
echo ""
echo ""

# Test 3: List Models
echo "Test 3: List Models"
echo "-----------------------------------"
curl -s "$GATEWAY_URL/v1/models" | jq '.data[0:2]'
echo ""
echo ""

# Test 4: Chat Completion (Non-streaming)
echo "Test 4: Chat Completion (Non-streaming)"
echo "-----------------------------------"
curl -s "$GATEWAY_URL/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $API_KEY" \
  -d '{
    "model": "meta-llama/llama-3.1-8b-instruct:free",
    "messages": [
      {"role": "user", "content": "Say hello in 5 words"}
    ]
  }' | jq '.choices[0].message.content // .error'
echo ""
echo ""

# Test 5: Chat Completion (Streaming)
echo "Test 5: Chat Completion (Streaming)"
echo "-----------------------------------"
echo "Sending streaming request (first 5 chunks)..."
curl -s -N "$GATEWAY_URL/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $API_KEY" \
  -d '{
    "model": "meta-llama/llama-3.1-8b-instruct:free",
    "messages": [
      {"role": "user", "content": "Count from 1 to 3"}
    ],
    "stream": true
  }' | head -n 5
echo ""
echo ""

# Test 6: Sticky Session
echo "Test 6: Sticky Session (Same conversation_id)"
echo "-----------------------------------"
CONV_ID="test-conv-$(date +%s)"
echo "Conversation ID: $CONV_ID"
echo "Request 1:"
curl -s "$GATEWAY_URL/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $API_KEY" \
  -d "{
    \"model\": \"meta-llama/llama-3.1-8b-instruct:free\",
    \"messages\": [
      {\"role\": \"user\", \"content\": \"Hello\"}
    ],
    \"conversation_id\": \"$CONV_ID\"
  }" | jq '.choices[0].message.content // .error' || echo "Request failed"
  
echo "Request 2 (should use same proxy):"
curl -s "$GATEWAY_URL/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $API_KEY" \
  -d "{
    \"model\": \"meta-llama/llama-3.1-8b-instruct:free\",
    \"messages\": [
      {\"role\": \"user\", \"content\": \"Hi again\"}
    ],
    \"conversation_id\": \"$CONV_ID\"
  }" | jq '.choices[0].message.content // .error' || echo "Request failed"
echo ""
echo ""

echo "==================================="
echo "All tests completed!"
echo "==================================="
