# GoReleaser supplies the prebuilt binary; this image only packages it.
#
# The build context holds artifacts under platform directories (linux/amd64,
# linux/arm64), so the copy is resolved through TARGETPLATFORM rather than a
# bare filename — that is what makes one Dockerfile serve a multi-arch build.
FROM gcr.io/distroless/static-debian12:nonroot

ARG TARGETPLATFORM
COPY $TARGETPLATFORM/cmemlan /usr/local/bin/cmemlan

VOLUME ["/data"]
EXPOSE 8787

# A container must bind the wildcard to be reachable at all, which necessarily
# overrides the bind guard. The peer allowlist is what remains, so the binary
# refuses to start without it when this is set. (An exec-form entrypoint cannot
# expand environment variables, and distroless has no shell to do it, so the
# requirement is enforced in the binary rather than here.)
ENV CMEMLAN_REQUIRE_ALLOW_CIDR=1

# Publish to loopback — `docker run -p 127.0.0.1:8787:8787` — because Docker's
# port publishing installs DNAT rules that bypass ufw and firewalld.
ENTRYPOINT ["/usr/local/bin/cmemlan", "serve", \
            "--data-dir", "/data", \
            "--bind", "0.0.0.0:8787", \
            "--insecure-public-bind"]
