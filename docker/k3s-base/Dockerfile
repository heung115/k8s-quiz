FROM rancher/k3s:v1.36.4-k3s1@sha256:edad48e12bf81c3a09ac1c05c0c0ffaaa22145980b989d6fae84543a76b83657

# rancher/k3s is a minimal image with no package manager (no apk/apt).
# It ships k3s + kubectl + sh, which is all setup/verify scripts need
# (they run curl etc. inside Pods, not on this host). Keep it minimal.
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
CMD ["server"]
