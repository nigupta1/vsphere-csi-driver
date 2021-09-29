ARG OSVERSION
ARG ARCH=amd64
FROM --platform=linux/amd64 gcr.io/k8s-staging-e2e-test-images/windows-servercore-cache:1.0-linux-${ARCH}-${OSVERSION} as core

FROM mcr.microsoft.com/windows/nanoserver:${OSVERSION}
COPY --from=core /Windows/System32/netapi32.dll /Windows/System32/netapi32.dll

USER ContainerAdministrator
LABEL description="CSI plugin"


COPY .build/bin/vsphere-csi.windows_amd64 /csi.exe
ENTRYPOINT ["/csi.exe"]