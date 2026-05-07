package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"k8s.io/client-go/tools/clientcmd"

	goldenimage "github.com/redhat-openshift-partner-labs/udn-cdi-local-upload-workaround"
)

type imageList []goldenimage.ImageSpec

func (l *imageList) String() string {
	var parts []string
	for _, img := range *l {
		parts = append(parts, img.Name+":"+img.Path)
	}
	return strings.Join(parts, ", ")
}

func (l *imageList) Set(val string) error {
	idx := strings.Index(val, ":")
	if idx < 1 || idx == len(val)-1 {
		return fmt.Errorf("invalid format %q, expected name:path", val)
	}
	*l = append(*l, goldenimage.ImageSpec{
		Name: val[:idx],
		Path: val[idx+1:],
	})
	return nil
}

func main() {
	kubeconfig := flag.String("kubeconfig", os.Getenv("KUBECONFIG"), "Path to kubeconfig file")
	namespace := flag.String("namespace", "", "Target namespace for golden image")
	storageClass := flag.String("storage-class", "", "Storage class (optional)")
	timeout := flag.Duration("timeout", 2*time.Hour, "Upload timeout duration (e.g. 30m, 3h)")

	var images imageList
	flag.Var(&images, "image", "Image to upload as name:path (repeatable)")

	flag.Parse()

	if *namespace == "" || len(images) == 0 {
		fmt.Println("Usage: golden-image-upload --namespace <ns> --image <name>:<path> [--image <name>:<path> ...]")
		flag.PrintDefaults()
		os.Exit(1)
	}

	for i := range images {
		if images[i].Size == "" {
			detectedSize, err := goldenimage.GetPVCSize(images[i].Path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error detecting image size for %s: %v\n", images[i].Path, err)
				os.Exit(1)
			}
			fmt.Printf("Auto-detected PVC size for %s: %s\n", images[i].Name, detectedSize)
			images[i].Size = detectedSize
		}
	}

	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error building kubeconfig: %v\n", err)
		os.Exit(1)
	}

	uploader, err := goldenimage.NewGoldenImageUploader(
		config,
		*namespace,
		images,
		*storageClass,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating uploader: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if err := uploader.Upload(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error uploading image: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Upload completed successfully!")
}
