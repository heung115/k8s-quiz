FROM rancher/k3s:v1.36.3-k3s1@sha256:d0f79175794edd9694b4a12bafc5c52ae1977369a2f7cf256264e7bd2dae0be9

# rancher/k3s is a minimal image with no package manager (no apk/apt).
# It ships k3s + kubectl + sh, which is all setup/verify scripts need
# (they run curl etc. inside Pods, not on this host). Keep it minimal.
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
CMD ["server"]
