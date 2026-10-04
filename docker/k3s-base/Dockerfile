FROM rancher/k3s:v1.37.1-k3s1@sha256:ca7f37d993d82ef0dcdcfecb2e0e2618ea541dbaffc620c8cedebe01a82acd0d

# rancher/k3s is a minimal image with no package manager (no apk/apt).
# It ships k3s + kubectl + sh, which is all setup/verify scripts need
# (they run curl etc. inside Pods, not on this host). Keep it minimal.
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
CMD ["server"]
