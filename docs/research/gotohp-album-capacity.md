# gotohp album capacity follow-up

Recorded from FoxyPhotos provider research on 2026-11-06; reference revision
f6ad6b63ec67694011c686f246f8832cb0e6fb9a. Research only; no implementation or
live account tests, no implicit rollout or album rollover policy.

Authoritative sources retrieved 2026-11-06:
- https://support.google.com/photos/answer/6128849?hl=en: albums allow up to 20,000 photos or videos.
- https://developers.google.com/photos/library/guides/manage-albums: each album has a 20,000-media-item limit; exceeding requests fail.
- https://support.google.com/photos/answer/6131416?hl=en: separately documents 20,000 photos in conversations. A conversation is not a shared album.

Photos and videos share an album item bound; account storage entitlement does not
remove it. No distinct shared-album limit or native endpoint error schema was
established. Do not fill a live album to probe this boundary.

At the pinned source, gotohp.go addToAlbum discards refreshLibraryCache errors,
resolves a title-to-key cache and creates when absent. The map collapses duplicate
titles; resolve/create is not atomic. api/client.go AddMediaToAlbum checks HTTP
success and discards response bytes. There is no typed album-capacity outcome,
preflight confirmed count, capacity error fixture or membership verification.

Required before claiming safe folder/album association at capacity:
- Capture sanitized native error-shape fixtures with explicit authorization.
- Model full/unknown capacity and uncertain membership separately from successful upload.
- Fail closed on cache refresh errors and ambiguous titles; serialize resolve/create.
- Retain confirmed uploaded media identity when album association fails, so retries do not reupload.
- Do not loop, create duplicate albums, claim membership from HTTP alone, or delete originals.
- Surface a terminal capacity result. Rollover/naming requires a separate documented decision.

No bclone implementation changes are part of the FoxyPhotos milestone.
