# Runtime image of a release. goreleaser builds the binary (CGO_ENABLED=0,
# the version linked in) and puts it in the build context under
# $TARGETPLATFORM; nothing is compiled here. See docs/development.md#releasing.
#
# distroless/static carries only what a static binary needs from the system:
# the CA bundle (nops calls git, OIDC, Nomad and notification endpoints over
# TLS), tzdata and a non-root user (nonroot, uid 65532). Pinned by digest;
# Dependabot bumps it.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99

ARG TARGETPLATFORM
COPY $TARGETPLATFORM/nops /nops

# Writable by nonroot, so the default -db-path (nops.db) works in a
# throwaway container; a real deployment mounts a volume and sets
# NOPS_DB_PATH (docs/configuration.md#running-nops-as-a-nomad-job).
WORKDIR /home/nonroot
USER nonroot:nonroot
EXPOSE 8080
# The image starts the server: "nops serve" is the one command that is not the
# client of the API (docs/cli.md). A command given to docker run, such as
# "approve" or "-version", replaces it.
ENTRYPOINT ["/nops"]
CMD ["serve"]
