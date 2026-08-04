FROM rancher/k3s:v1.29.3-k3s1@sha256:2a91a826b6bfb5695d0c8978a6d7526bce72a06a1c4e6a09cb96a75d638b1f36

# rancher/k3s is a minimal image with no package manager (no apk/apt).
# It ships k3s + kubectl + sh, which is all setup/verify scripts need
# (they run curl etc. inside Pods, not on this host). Keep it minimal.
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
CMD ["server"]
