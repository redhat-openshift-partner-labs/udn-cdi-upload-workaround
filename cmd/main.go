package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/client-go/tools/clientcmd"

	"github.com/redhat-openshift-partner-labs/udn-cdi-local-upload-workaround"
)

func main() {
	// Parse command line flags
	kubeconfig := flag.String("kubeconfig", os.Getenv("KUBECONFIG"), "Path to kubeconfig file")
	namespace := flag.String("namespace", "", "Target namespace for golden image")
	pvcName := flag.String("name", "", "Name for the DataVolume/PVC")
	pvcSize := flag.String("size", "", "Size of the PVC (auto-detected from image if not set)")
	storageClass := flag.String("storage-class", "", "Storage class (optional)")
	imagePath := flag.String("image-path", "", "Path to local disk image")

	flag.Parse()

	// Validate required flags
	if *namespace == "" || *pvcName == "" || *imagePath == "" {
		fmt.Println("Usage: golden-image-upload --namespace <ns> --name <name> --image-path <path>")
		flag.PrintDefaults()
		os.Exit(1)
	}

	// Auto-detect PVC size from image if not specified
	if *pvcSize == "" {
		detectedSize, err := goldenimage.GetPVCSize(*imagePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error detecting image size: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Auto-detected PVC size: %s\n", detectedSize)
		*pvcSize = detectedSize
	}

	// Build rest config
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error building kubeconfig: %v\n", err)
		os.Exit(1)
	}

	// Create uploader
	uploader, err := goldenimage.NewGoldenImageUploader(
		config,
		*namespace,
		*pvcName,
		*pvcSize,
		*storageClass,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating uploader: %v\n", err)
		os.Exit(1)
	}

	// Run upload with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	if err := uploader.Upload(ctx, *imagePath); err != nil {
		fmt.Fprintf(os.Stderr, "Error uploading image: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Upload completed successfully!")
}
