# M8 — Docker install path (design contract)

The systemd path (preflight → install → activation → cleanup) is accepted on real
hosts. This slice adds the **Docker container** alternative: the same operator flow
and the same authority model, with the probe running as a container from a
digest-pinned image. It reuses the existing verified-image seams for metadata,
content, snapshots and the closed OCI archive.

## Decisions

1. **Identity.** `ProbeInstallIdentity{Mode: docker, Image: ProbeImageSelection}`.
   - Journal 084 already accepts this mode and the resource layout
     `[image_cache (not owned), data_directory, container]`.
   - `Binary` stays zero.
   - The operator submits only the image **pin**: repository, version, architecture,
     index SHA-256 and size. The hub resolves the selection (manifest, config, layer
     count) by verifying the operator's local OCI layout before any durable write.
2. **Artifacts.** `PROBE_INSTALL_ARTIFACT_DIR/<pin.ReleaseDirName()>/` is an OCI image
   layout that holds at least `blobs/sha256/<digest>` for:
   - the pinned index;
   - the selected manifest;
   - the config;
   - every layer.

   A small reader opens those blobs by digest, and the existing content verifier is
   authoritative over them. The verified snapshot is then packed by the existing
   closed archive packer.
3. **Wire.** Host actions for Docker identities use frame v2:
   - The action frame is `PHXACT2\n` + kind length + kind + `PHXDKR1\n` + u32 length +
     a strict JSON Docker header (identity, owner, attempt).
   - Systemd identities keep v1 unchanged, and the helper accepts both.
   - Grant frames, challenges and receipts are unchanged.
4. **Marker.** For Docker identities the marker lists the image fields instead of the
   binary fields. The `container` kind binds `container_sha256`: the hash of the
   exact derived `docker create` argument vector, the analogue of `unit_sha256`.
5. **`image_cache` payload.**
   - **Intent.** The `image_cache` intent carries
     `Payload{SHA256, Size, LayoutIndexSHA256}`: the closed archive's descriptor.
     The marker and grant bind it, and every other kind must leave it zero.
   - **Wire.** After the grant comes `PHXIMG1\n` + u64 size + the archive bytes.
   - **Staging.** The helper streams the bytes to a root-only `0700` staging
     directory (`/var/lib/uptime-phoenix-image-stage`) and verifies the exact
     SHA-256 and size before anything is loaded.
   - **Load and verify.** It then runs `docker load --input` and removes the file. It
     requires the image to resolve by `sha256:<config>` (classic store) or
     `sha256:<layout index>` (containerd store); any other result is Unknown.
   - **Tag.** It tags that image ID `uptime-phoenix-probe:<manifest hex>`.
   - **Ownership.** The image is shared (`Owned=false`); cleanup never removes it.
     Tags are local root state, and root on the host is trusted.
6. **Data directory.** The same root-owned `0700` `/var/lib/uptime-phoenix-probe-<op>`
   is used, and the same action applies.
7. **Container (privileges mirror the approved systemd unit).**
   `docker create --name uptime-phoenix-probe-<op>` is created but not started:
   - `--label io.uptime-phoenix.operation=<op>`;
   - `--restart unless-stopped`;
   - `--user 0:0 --cap-drop ALL --cap-add NET_RAW`;
   - `--security-opt no-new-privileges --read-only --tmpfs /tmp`;
   - `--publish 8443:8443`;
   - `--mount type=bind,src=/var/lib/uptime-phoenix-probe-<op>,dst=/var/lib/uptime-phoenix/probe`;
   - image `uptime-phoenix-probe:<manifest hex>`, command
     `run --data-dir /var/lib/uptime-phoenix/probe`.

   An existing container with that name is a Conflict.
8. **Activation** (same four steps).
   - **`probe_init`:**
     `docker run --rm --network none --user 0:0 --cap-drop ALL --read-only` plus the
     same data mount, then `init --data-dir …`. It requires an empty data directory
     and parses the same probe-init receipt.
   - **`service_start`:** `docker start`, then poll `docker inspect` until it is
     running (20 s).
   - Step references come from a mode-aware layout helper, not fixed indexes.
9. **Cleanup.** `docker rm --force` runs only if the container's operation label
   matches; otherwise Conflict. Then the data tree is removed and absence verified.
   The image is retained.
10. **Preflight.**
    - `docker=1` now means a `docker` CLI and a `/var/run/docker.sock` socket.
    - Docker mode checks architecture against the image pin and requires `docker=1`
      instead of systemd.
11. **API.**
    - `POST /api/probe-installs` takes an optional `mode` (`systemd` by default).
    - Docker requires `image_repository`, `image_version`, `image_architecture`,
      `image_index_sha256` and `image_index_size`, and refuses `binary_*`.
    - Views add `image {repository, version, architecture}` for Docker, and activation
      and cleanup accept Docker installs.
12. **Persistence.** Migration `089` widens the host-action admission kinds with
    `image_cache` and `container`. Activation (087) and cleanup (088) need no schema
    change.

## Increments and ownership

| Increment | Owner | Files |
|---|---|---|
| D1 contract | integrator | domain (layout helper, marker, Docker args, preflight refusal), ports |
| D2 host side | agent | frame v2 decode, payload read, Docker appliers (fake `ProbeDocker` runner), Linux wiring |
| D3 persistence | agent | migration 089, stores that assume systemd, gate leaves |
| D4 hub side | integrator | sender v2 frame and payload, executors, OCI layout reader and session factory, service/API, preflight |
| D5 acceptance | integrator | AWS hosts with `docker.io`, OCI images built from `Dockerfile.probe`, the matrix extended for Docker |
