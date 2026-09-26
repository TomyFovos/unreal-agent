# Credentials

Reference is the only credential value for Session/Operation/Host metadata.
Secret redacts JSON and formatting; Reveal is for the storage/HTTP boundary only.
Store is replaceable. Its WithCredential transaction contract includes account
locking across processes, atomic commit, and reread while locked. Manager owns
expiry and refresh coordination; RefreshFunc belongs to the provider. Supply the
Host permission context and an enforced HTTP transport to provider refresh code.

Local is implemented on Linux/macOS using private 0700 directories, regular
0600 files, ownership/link checks, O_NOFOLLOW, flock, atomic rename and fsync.
Unsupported backends/platforms fail explicitly. Lock files are retained so
unlink cannot split ownership. Metadata listing never returns token material.

Every refresh writes refresh_pending before calling the provider once. The
account lock spans reread, marker, exchange and final durable save. A crashed or
failed exchange/save leaves pending state requiring reauthentication, rather
than reusing a possibly consumed rotating token. Logout acquires the same lock:
after logout returns, an older refresh cannot resurrect the record. Login is an
explicit new credential installation. External ownership never refreshes here.

There is no reactive 401 model replay. A model request obtains current material
before its first HTTP call; server rejection is a normalized error. Avoiding
model side-effect duplication takes precedence over retrying authentication.

Tests use generated synthetic secrets, five independent OS processes, a local
rotating-token server, access-token verification for every child, logout races,
cancellation, private-permission rejection, expiry, ownership and injected
post-refresh persistence failure. No live provider account is needed.
