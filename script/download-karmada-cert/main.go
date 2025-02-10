package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	// 定义命令行参数
	var (
		kubeconfig string
		namespace  string
		outputDir  string
		secretName string
	)

	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig file")
	flag.StringVar(&namespace, "namespace", "", "Karmada namespace")
	flag.StringVar(&outputDir, "output-dir", ".", "Directory to save certificate files")
	flag.StringVar(&secretName, "secret-name", "karmada-cert", "Name of the secret to export (default: karmada-cert)")
	flag.Parse()

	// 验证必要参数
	if kubeconfig == "" || namespace == "" {
		log.Fatal("--kubeconfig and --namespace are required")
	}

	// 创建 kubernetes 客户端
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		log.Fatalf("Error building kubeconfig: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Error creating kubernetes client: %v", err)
	}

	// 获取 karmada-cert secret
	secret, err := clientset.CoreV1().Secrets(namespace).Get(context.TODO(), secretName, metav1.GetOptions{})
	if err != nil {
		log.Fatalf("Error getting secret %s: %v", secretName, err)
	}

	// 创建输出目录
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		log.Fatalf("Error creating output directory: %v", err)
	}

	// 遍历 secret data 并保存文件
	for filename, data := range secret.Data {
		outputPath := filepath.Join(outputDir, filename)
		if err := os.WriteFile(outputPath, data, 0644); err != nil {
			log.Fatalf("Error writing file %s: %v", outputPath, err)
		}
		fmt.Printf("Saved certificate file: %s\n", outputPath)
	}

	fmt.Println("Successfully exported all certificate files")
}
