# Vision Authentication

Vision supports opt-in bearer token authentication for MCP streamable endpoints. By default, Vision runs in **open access** mode (no authentication required) — appropriate for single-machine localhost deployments.

## Enabling Authentication

Add a `security` section to your `servers.yaml`:

```yaml
security:
  bearer_token: "your-secret-token-here"
  allowed_origins:
    - "http://localhost:3000"
```

### Environment Variable

For production deployments, use environment variable expansion to avoid committing secrets:

```yaml
security:
  bearer_token: "${VISION_BEARER_TOKEN}"
```

Set the environment variable before starting Vision:

```bash
export VISION_BEARER_TOKEN="your-secret-token-here"
vision daemon start
```

## How It Works

When `bearer_token` is set, Vision's MCP proxy middleware validates the `Authorization` header on every request to streamable MCP endpoints:

1. Client sends `Authorization: Bearer <token>` header
2. Middleware compares against the configured `bearer_token`
3. Requests with missing or invalid tokens are rejected with `401 Unauthorized`
4. No session or subprocess is created for unauthorized requests

When `bearer_token` is empty (default), the middleware passes all requests through without authentication.

### Origin Checking

The `allowed_origins` list enforces strict CORS origin validation:

- Empty list: origin checking is not enforced
- Wildcard (`*`): explicitly rejected (not allowed)
- Only explicitly listed origins are permitted

## Security Considerations

### Single-Machine (Default)

Vision's primary use case is single-machine localhost deployment:

- All MCP endpoints listen on `localhost` (`127.0.0.1`) only
- No network exposure by default
- Bearer token authentication is optional

### Network-Exposed Deployments

If you expose Vision to a network (not recommended without additional hardening):

- **Always** set `bearer_token` to a strong random value
- Set `allowed_origins` to restrict CORS to known clients
- Use a reverse proxy (nginx, caddy) for TLS termination
- Consider firewall rules to limit access

## Troubleshooting

### "401 Unauthorized" errors

- Verify the client sends `Authorization: Bearer <token>` header
- Check that the token matches exactly (no trailing whitespace)
- If using `${VISION_BEARER_TOKEN}`, verify the environment variable is set

### Status warning: "No bearer_token configured"

Run `vision status` — if you see:

```
⚠ No bearer_token configured — see docs/AUTH.md for secure setup
```

This is informational for single-machine deployments. To suppress it, set any `bearer_token` value in your `servers.yaml`.

### Token not expanding from environment

Ensure the `${VAR}` syntax is used (not `$VAR`). Only `${VAR}` expansion is supported.
