FROM rancher/k3s:v1.37.2-k3s1@sha256:21ea6e0fac4e3930c2ce8aa40ddd41a8b18cb3e9a0222c030dc005e7e88c4332

# rancher/k3s is a minimal image with no package manager (no apk/apt).
# It ships k3s + kubectl + sh, which is all setup/verify scripts need
# (they run curl etc. inside Pods, not on this host). Keep it minimal.
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
CMD ["server"]
