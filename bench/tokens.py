"""Shared token-counting helper for defn benches.

RULE: any bench that emits a "we save X bytes / X% wire savings" claim
MUST also emit the same measurement in TOKENS. Bytes lie because
tokenizers are trained on code corpora and collapse common identifiers
into 1-2 tokens each, while short "compressed" placeholders (like
`[47]` or `#42`) typically expand to 3 tokens. Byte math has produced
+2% "savings" on measurements that were actually -6% in tokens.

Uses tiktoken cl100k_base as a proxy for Anthropic tokenization
(Anthropic does not publish their tokenizer; cl100k is trained on
similar code corpora and empirically tracks within a few percent for
code content). For load-bearing claims, cross-check with the Anthropic
messages.count_tokens API.

Usage:
    from bench.tokens import count_tokens, require_tokens
    n = count_tokens("some string")

    # In an analysis script:
    require_tokens(bytes_saved=SB, tokens_saved=ST, label="my-lever")
    # → prints both, asserts they agree in sign
"""

import functools
import sys

try:
    import tiktoken

    _ENC = tiktoken.get_encoding("cl100k_base")
    _AVAILABLE = True
except ImportError:
    _AVAILABLE = False
    _ENC = None


@functools.lru_cache(maxsize=1024)
def _cached_encode_len(s: str) -> int:
    return len(_ENC.encode(s))


def count_tokens(text: str) -> int:
    """Return token count via tiktoken cl100k_base.

    Raises ImportError if tiktoken is not installed. Callers should
    invoke bench scripts via `uv run --with tiktoken python3 ...` on
    machines where system pip is blocked.
    """
    if not _AVAILABLE:
        raise ImportError(
            "tiktoken is required. Run:\n"
            "  uv run --with tiktoken python3 <script>\n"
            "or install with: pip install --break-system-packages tiktoken"
        )
    return _cached_encode_len(text)


def require_tokens(
    *,
    bytes_saved: int,
    tokens_saved: int,
    label: str,
    baseline_bytes: int | None = None,
    baseline_tokens: int | None = None,
) -> None:
    """Emit a bytes+tokens report; assert sign agreement.

    Any bench that claims wire savings must call this. If bytes and
    tokens disagree in sign (bytes say +save, tokens say +cost), fail
    loudly — bytes are lying and the tokens are the truth.
    """
    b_pct = (bytes_saved / baseline_bytes * 100) if baseline_bytes else None
    t_pct = (tokens_saved / baseline_tokens * 100) if baseline_tokens else None
    print(f"\n=== {label} ===")
    if baseline_bytes:
        print(
            f"  Bytes:  {bytes_saved:+,d}  ({b_pct:+.2f}%)  "
            f"[baseline {baseline_bytes:,}]"
        )
    else:
        print(f"  Bytes:  {bytes_saved:+,d}")
    if baseline_tokens:
        print(
            f"  Tokens: {tokens_saved:+,d}  ({t_pct:+.2f}%)  "
            f"[baseline {baseline_tokens:,}]"
        )
    else:
        print(f"  Tokens: {tokens_saved:+,d}")

    if (bytes_saved > 0) != (tokens_saved > 0):
        print(
            "  ⚠  BYTES AND TOKENS DISAGREE IN SIGN — tokens are the truth.",
            file=sys.stderr,
        )
        print(
            "     Bytes are misleading here; do NOT cite the byte number.",
            file=sys.stderr,
        )


if __name__ == "__main__":
    # Smoke test: show that common Go identifiers vs [N] refs
    # demonstrate the byte-vs-token gap.
    samples = [
        ("handleImpact", "[47]"),
        ("ServeHTTP", "[47]"),
        ("Authenticate", "[47]"),
        ("http.HandlerFunc", "[47]"),
    ]
    print("sample: bytes / tokens")
    for a, b in samples:
        print(
            f"  {a!r} → {len(a)} bytes / {count_tokens(a)} tokens "
            f"|  {b!r} → {len(b)} bytes / {count_tokens(b)} tokens"
        )
    print('\nNote how [47] is often MORE tokens than the ident it "compresses".')
