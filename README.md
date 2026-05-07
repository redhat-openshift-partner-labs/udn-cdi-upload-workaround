# UDN CDI Local Upload Workaround

Upload local disk images as golden images to OpenShift namespaces that use a Primary User-Defined Network (UDN), where the standard `virtctl image-upload` / CDI uploadproxy workflow is blocked by network isolation.

## The Problem

When a namespace is configured with a Primary UDN (Layer2 or Layer3), pods get their primary interface on the UDN subnet rather than the default cluster network. The CDI upload proxy runs in `openshift-cnv` on the default cluster network and cannot reach the `cdi-upload` pod because:

1. The upload pod's primary interface is on the UDN (e.g. `192.168.100.x`)
2. Its `eth0` (default network) is infrastructure-locked -- OVN ACLs restrict it to kubelet traffic only
3. No network path exists between the upload proxy and the upload pod

See [udn-golden-image-workflow.md](udn-golden-image-workflow.md) for detailed architecture diagrams.

## How It Works

Instead of going through the CDI upload proxy, this tool:

1. Detects whether the target namespace has a Primary UDN (via namespace labels + ClusterUserDefinedNetwork/UserDefinedNetwork CRs)
2. Creates an ephemeral nginx pod and ClusterIP service inside the target namespace (on the UDN)
3. For each image: streams it to the nginx pod via `exec/tar` (tunneled through the API server, bypassing OVN), with progress reporting and SHA256 checksum verification (auto-retries up to 3 times on mismatch)
4. Creates a DataVolume with an HTTP source pointing at the in-namespace nginx service
5. The CDI importer pod fetches the image over the UDN (pod-to-pod on the same L2 segment)
6. Cleans up the ephemeral pod and service

For namespaces without a Primary UDN, the tool exits with a message to use `virtctl image-upload` instead.

## Prerequisites

- Access to an OpenShift cluster with OpenShift Virtualization (CNV) installed
- CDI (Containerized Data Importer) CRDs available (`datavolumes.cdi.kubevirt.io`)
- `kubeconfig` with permissions to create pods, services, and DataVolumes in the target namespace
- A local disk image file (e.g. `.qcow2`, `.raw`, `.img`)

## Build

```bash
go mod tidy
go build -o udn-image-uploader ./cmd
```

## Usage

```bash
./udn-image-uploader \
  --namespace <namespace> \
  --image <name>:<path> \
  [--image <name>:<path> ...] \
  [--storage-class <sc>] \
  [--timeout <duration>] \
  [--kubeconfig <path>]
```

### Flags

| Flag | Required | Default | Description |
|------|----------|---------|-------------|
| `--namespace` | Yes | | Target namespace for the golden image |
| `--image` | Yes | | Image to upload as `name:path` (repeatable for multiple images) |
| `--storage-class` | No | cluster default | StorageClass for the PVC |
| `--timeout` | No | `2h` | Upload timeout duration (e.g. `30m`, `3h`) |
| `--kubeconfig` | No | `$KUBECONFIG` | Path to kubeconfig file |

PVC size is auto-detected from each image file (file size + 20% overhead, rounded up to the nearest Gi).

### Example

```bash
# Single image
./udn-image-uploader \
  --namespace green-namespace \
  --image golden-image:./Fedora-Cloud-Base-Generic-43-1.6.x86_64.qcow2 \
  --storage-class gp3-csi

# Multiple images in a single run
./udn-image-uploader \
  --namespace green-namespace \
  --image fedora-43:./Fedora-Cloud-Base-Generic-43-1.6.x86_64.qcow2 \
  --image rhel-9:./rhel-9.5-x86_64-kvm.qcow2 \
  --storage-class gp3-csi \
  --timeout 3h

# Non-UDN namespace -- exits with guidance to use virtctl
./udn-image-uploader \
  --namespace standard-namespace \
  --image golden-image:./Fedora-Cloud-Base-Generic-43-1.6.x86_64.qcow2
```

### Example Output

```
Auto-detected PVC size for fedora-43: 1Gi
Detected Primary UDN in namespace green-namespace, using HTTP source workflow
Creating ephemeral image server pod...
Creating image server service...

[1/1] Uploading image "fedora-43" from ./Fedora-Cloud-Base-Generic-43-1.6.x86_64.qcow2
Computing local checksum for ./Fedora-Cloud-Base-Generic-43-1.6.x86_64.qcow2...
Local SHA256: a1b2c3d4...
Streaming image ./Fedora-Cloud-Base-Generic-43-1.6.x86_64.qcow2 to pod...
Image size: 583335936 bytes (0.54 GB)
[ 45.0%] 250 MB / 556 MB  (48.3 MB/s)
[100.0%] 556 MB / 556 MB  (52.1 MB/s)
Wrote 583335936 bytes to tar stream
Verifying upload checksum...
Checksum verified successfully
Creating DataVolume with HTTP source...
Waiting for DataVolume to complete...
DataVolume phase: ImportScheduled
DataVolume phase: ImportInProgress
DataVolume phase: Succeeded
Golden image fedora-43 created successfully
Cleaning up ephemeral resources...
Upload completed successfully!
```

## Alternatives

| Method | Works with UDN? | Notes |
|--------|-----------------|-------|
| `virtctl image-upload` / CDI uploadproxy | No | Blocked by UDN network isolation |
| **This tool** (HTTP source + ephemeral server) | Yes | Self-contained, no external infra needed |
| Registry source DataVolume | Yes | Requires a container registry accessible from the cluster |
| S3 source DataVolume | Yes | Requires S3-compatible storage accessible from the cluster |

## TODO

- [ ] Support resumable uploads for large images
