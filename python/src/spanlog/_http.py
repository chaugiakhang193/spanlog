from __future__ import annotations

import re
import secrets

DEFAULT_REQUEST_ID_HEADER = "x-request-id"
SAFE_PATH_CHARACTERS = "/:@!$&'()*+,;=~"

# The request id lands on every line of the request. An id from a client is kept only
# when it cannot break a line or flood it.
_SAFE_REQUEST_ID = re.compile(r"[A-Za-z0-9._-]{1,64}")


def safe_request_id(value: str | None) -> str | None:
    if value is not None and _SAFE_REQUEST_ID.fullmatch(value):
        return value
    return None


def new_request_id() -> str:
    return secrets.token_hex(4)
