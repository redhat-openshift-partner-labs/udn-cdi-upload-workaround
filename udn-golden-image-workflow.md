# Golden Image Upload with Primary User-Defined Networks

## Why Standard Upload Workflows Fail

```mermaid
flowchart TB
    subgraph problem["❌ THE PROBLEM: Network Plane Mismatch"]
        direction TB
        
        subgraph cnv["openshift-cnv namespace"]
            proxy["cdi-uploadproxy<br/>IP: 10.128.x.x<br/>(Default Cluster Network)"]
        end
        
        subgraph green["green-namespace (Primary UDN)"]
            subgraph uploadpod["cdi-upload pod"]
                eth0["eth0: 10.129.x.x<br/>🔒 Infrastructure-Locked<br/>(Kubelet healthchecks only)"]
                udn["ovn-udn1: 192.168.100.x<br/>✅ Primary Interface<br/>(All application traffic)"]
            end
        end
        
        proxy -->|"❌ BLOCKED<br/>OVN ACLs + nftables<br/>deny non-kubelet traffic"| eth0
        proxy -.->|"❌ UNREACHABLE<br/>Different network plane"| udn
    end
    
    subgraph root["🔍 Root Cause Analysis"]
        r1["1. cdi-uploadproxy runs on<br/>default cluster network"]
        r2["2. Upload pod's primary interface<br/>is on Layer2 UDN"]
        r3["3. eth0 is infrastructure-locked<br/>by UDN isolation rules"]
        r4["4. No network path exists<br/>between proxy and upload pod"]
        
        r1 --> r2 --> r3 --> r4
    end
    
    problem --> root
```

## UDN Detection Logic

```mermaid
flowchart TD
    start([Start: Upload Golden Image]) --> getns["Get target namespace"]
    
    getns --> checklabel{"Namespace has label?<br/><code>k8s.ovn.org/primary-user-defined-network</code>"}
    
    checklabel -->|No| standard["Use Standard Upload Flow<br/>(virtctl / CDI uploadproxy)"]
    checklabel -->|Yes| checkcudn["Query ClusterUserDefinedNetworks"]
    
    checkcudn --> cudnmatch{"Any CUDN with:<br/>1. namespaceSelector matches ns<br/>2. role: Primary"}
    
    cudnmatch -->|Yes| httpflow["Use HTTP Source Workflow"]
    cudnmatch -->|No| checkudn["Query UserDefinedNetworks<br/>in namespace"]
    
    checkudn --> udnmatch{"Any UDN with<br/>role: Primary"}
    
    udnmatch -->|Yes| httpflow
    udnmatch -->|No| standard
    
    standard --> done([Complete])
    httpflow --> done
    
    style httpflow fill:#90EE90
    style standard fill:#87CEEB
```

## HTTP Source Workflow (Recommended Solution)

```mermaid
sequenceDiagram
    autonumber
    participant MCS as Citrix MCS
    participant K8s as Kubernetes API
    participant NS as green-namespace
    participant CDI as CDI Controller
    
    rect rgb(240, 248, 255)
        Note over MCS,CDI: Phase 1: Detection
        MCS->>K8s: Get namespace labels
        K8s-->>MCS: labels including k8s.ovn.org/primary-user-defined-network
        MCS->>K8s: List ClusterUserDefinedNetworks
        K8s-->>MCS: CUDN with namespaceSelector matching green-namespace
        MCS->>MCS: Detected Primary UDN → Use HTTP workflow
    end
    
    rect rgb(255, 250, 240)
        Note over MCS,NS: Phase 2: Setup Ephemeral HTTP Server
        MCS->>K8s: Create Pod (nginx:alpine) in green-namespace
        K8s->>NS: Pod scheduled on Layer2 UDN (192.168.100.x)
        MCS->>K8s: Create Service (ClusterIP) for Pod
        MCS->>MCS: Wait for Pod Ready
    end
    
    rect rgb(240, 255, 240)
        Note over MCS,NS: Phase 3: Transfer Image
        MCS->>K8s: Exec tar stream to Pod (via API server)
        Note right of K8s: API server tunnels through<br/>kubelet to pod namespace<br/>(bypasses OVN network)
        K8s->>NS: Image written to /usr/share/nginx/html/
    end
    
    rect rgb(255, 240, 245)
        Note over MCS,CDI: Phase 4: Create DataVolume
        MCS->>K8s: Create DataVolume with HTTP source<br/>url: http://mcs-image-server.green-namespace.svc/disk.qcow2
        K8s->>CDI: DataVolume created
        CDI->>NS: Spawn cdi-importer pod (on same UDN)
        NS->>NS: Importer fetches image from nginx<br/>(pod-to-pod on 192.168.100.0/24)
        NS->>NS: Image written to PVC
        CDI-->>K8s: DataVolume status: Succeeded
    end
    
    rect rgb(245, 245, 245)
        Note over MCS,NS: Phase 5: Cleanup
        MCS->>K8s: Delete Service
        MCS->>K8s: Delete Pod
        MCS-->>MCS: Golden image ready ✅
    end
```

## Network Flow Comparison

```mermaid
flowchart LR
    subgraph standard["❌ Standard Flow (Blocked)"]
        direction TB
        local1["Local Machine"] -->|"1. Upload"| proxy1["cdi-uploadproxy<br/>(openshift-cnv)"]
        proxy1 -->|"2. Forward<br/>❌ BLOCKED"| upload1["cdi-upload pod<br/>(green-namespace)"]
        upload1 -->|"3. Write"| pvc1["PVC"]
    end
    
    subgraph http["✅ HTTP Source Flow (Works)"]
        direction TB
        local2["Local Machine"] -->|"1. oc cp<br/>(via API server)"| nginx["nginx pod<br/>(green-namespace)<br/>192.168.100.x"]
        nginx -->|"2. HTTP GET<br/>✅ Same UDN"| importer["cdi-importer pod<br/>(green-namespace)<br/>192.168.100.y"]
        importer -->|"3. Write"| pvc2["PVC"]
    end
```

## Component Architecture

```mermaid
flowchart TB
    subgraph cluster["OpenShift Cluster"]
        subgraph cnvns["openshift-cnv namespace<br/>(Default Cluster Network)"]
            operator["cdi-operator"]
            controller["cdi-deployment<br/>(controller)"]
            apiserver["cdi-apiserver"]
            uploadproxy["cdi-uploadproxy<br/>🚫 Cannot reach UDN pods"]
        end
        
        subgraph greenns["green-namespace<br/>(Primary Layer2 UDN: 192.168.100.0/24)"]
            subgraph ephemeral["Ephemeral Resources (MCS creates)"]
                nginx["mcs-image-server<br/>(nginx pod)<br/>192.168.100.10"]
                svc["mcs-image-server<br/>(ClusterIP Service)"]
            end
            
            subgraph cdiresources["CDI Resources"]
                importer["cdi-importer pod<br/>192.168.100.11"]
                pvc["PVC<br/>(golden-image)"]
                dv["DataVolume<br/>source: HTTP"]
            end
            
            svc --> nginx
            importer -->|"HTTP GET<br/>(same L2 segment)"| svc
            importer --> pvc
            dv -.->|"triggers"| importer
        end
        
        subgraph cudn["ClusterUserDefinedNetwork"]
            cudnspec["namespaceSelector:<br/>- green-namespace<br/>- yellow-namespace<br/><br/>network:<br/>  topology: Layer2<br/>  role: Primary<br/>  subnet: 192.168.100.0/24"]
        end
        
        cudn -.->|"applies to"| greenns
        controller -.->|"manages"| dv
    end
    
    subgraph external["External"]
        mcs["Citrix MCS"]
        workstation["Admin Workstation"]
    end
    
    mcs -->|"1. Create pod/svc"| ephemeral
    mcs -->|"2. oc cp image"| nginx
    mcs -->|"3. Create DataVolume"| dv
    mcs -->|"4. Cleanup"| ephemeral
    
    workstation -->|"Alternative:<br/>virtctl (fails)"| uploadproxy
    
    style uploadproxy fill:#ffcccc
    style nginx fill:#ccffcc
    style importer fill:#ccffcc
```

## Decision Matrix

```mermaid
quadrantChart
    title Upload Method Selection
    x-axis Low Complexity --> High Complexity
    y-axis Requires External Infra --> Self-Contained
    quadrant-1 Avoid if possible
    quadrant-2 Best for UDN namespaces
    quadrant-3 Best for standard namespaces
    quadrant-4 Consider for CI/CD pipelines
    
    HTTP Source (ephemeral server): [0.25, 0.85]
    Standard Upload (virtctl): [0.2, 0.15]
    Registry Source: [0.6, 0.3]
    S3 Source: [0.7, 0.25]
    Port-Forward (manual): [0.5, 0.7]
```

## Implementation Checklist

```mermaid
flowchart TD
    subgraph checklist["Citrix MCS Implementation Checklist"]
        direction TB
        
        c1["☐ Add UDN detection logic<br/>(check namespace label + CUDN/UDN CRs)"]
        c2["☐ Implement ephemeral nginx pod creation"]
        c3["☐ Implement Service creation"]
        c4["☐ Implement image streaming via exec/tar"]
        c5["☐ Implement DataVolume creation with HTTP source"]
        c6["☐ Implement wait-for-completion logic"]
        c7["☐ Implement cleanup (delete pod + service)"]
        c8["☐ Add fallback to standard flow for non-UDN namespaces"]
        
        c1 --> c2 --> c3 --> c4 --> c5 --> c6 --> c7 --> c8
    end
```

## Summary

| Scenario | Method | Works with UDN? |
|----------|--------|-----------------|
| Standard namespace | `virtctl image-upload` / CDI uploadproxy | ✅ Yes |
| Primary UDN namespace | `virtctl image-upload` / CDI uploadproxy | ❌ No |
| Primary UDN namespace | HTTP source with ephemeral server | ✅ Yes |
| Primary UDN namespace | Registry source | ✅ Yes (requires registry) |
| Primary UDN namespace | S3 source | ✅ Yes (requires S3) |
