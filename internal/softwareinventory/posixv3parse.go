package softwareinventory

import (
	"net"
	"strconv"
	"strings"

	"ssh-manager-mcp/internal/models"
)

// posixV3Sections 是第 3 版固定命令集按顺序输出的全部节名。解析器用它发
// 现整节缺失的情况；命令声明 ABSENT 的节不算缺失。
var posixV3Sections = []string{
	"os", "packages", "listen", "listen-netstat", "listen-lsof", "exe",
	"exe-owner", "apps", "brew", "synology-packages", "container-images",
	"containers-v3", "container-image-refs", "container-image-labels",
	"firmware", "gpu-driver",
}

// buildPOSIXV3ContainerItems 以容器为条目单位构建第 3 版容器数据，并返回
// 未被任何容器使用的镜像行数。容器标识先经完整镜像标识关联到镜像行，再
// 按镜像标识读取镜像自带的版本标签。
func buildPOSIXV3ContainerItems(
	images, containers, imageRefs, imageLabels parsedSection,
) ([]models.SoftwareObservedItem, int) {
	if images.outcome == sectionAbsent ||
		containers.outcome == sectionAbsent || imageRefs.outcome == sectionAbsent {
		return nil, 0
	}

	type imageRow struct {
		repository, tag, digest, imageID string
	}
	containerImageIDs := map[string]string{}
	usedImageIDs := map[string]bool{}
	for _, line := range imageRefs.lines {
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		containerID := normalizeContainerImageID(fields[0])
		imageID := normalizeContainerImageID(fields[1])
		containerImageIDs[containerID] = imageID
		usedImageIDs[imageID] = true
	}

	imageRows := make([]imageRow, 0, len(images.lines))
	unusedImageCount := 0
	for _, line := range images.lines {
		fields := strings.Split(line, "\t")
		if len(fields) < 4 {
			continue
		}
		row := imageRow{
			repository: fields[0], tag: fields[1], digest: fields[2], imageID: fields[3],
		}
		if !usedImageIDs[normalizeContainerImageID(row.imageID)] {
			unusedImageCount++
		}
		imageRows = append(imageRows, row)
	}

	imageLabelsByVersion := map[string]string{}
	for _, line := range imageLabels.lines {
		imageID, version, present := strings.Cut(line, "\t")
		if !present || version == "<no value>" {
			version = ""
		}
		imageLabelsByVersion[normalizeContainerImageID(imageID)] = strings.TrimSpace(version)
	}

	rowsByImageID := map[string][]imageRow{}
	for _, row := range imageRows {
		imageID := normalizeContainerImageID(row.imageID)
		rowsByImageID[imageID] = append(rowsByImageID[imageID], row)
	}

	items := make([]models.SoftwareObservedItem, 0, len(containers.lines))
	for _, line := range containers.lines {
		fields := strings.Split(line, "\t")
		if len(fields) < 5 {
			continue
		}
		containerID, imageReference, containerName := fields[0], fields[1], fields[2]
		imageID := containerImageIDs[normalizeContainerImageID(containerID)]
		candidates := rowsByImageID[imageID]
		imageRowPresent := len(candidates) > 0
		if len(candidates) == 0 {
			candidates = []imageRow{{repository: imageReference}}
		}
		row := candidates[0]
		for _, candidate := range candidates {
			if containerReferenceMatchesImage(
				imageReference, candidate.repository,
				cleanContainerValue(candidate.tag), cleanContainerValue(candidate.digest),
				normalizeContainerImageID(candidate.imageID),
			) {
				row = candidate
				break
			}
		}
		tag, digest := cleanContainerValue(row.tag), cleanContainerValue(row.digest)
		repository := row.repository
		if imageRowPresent && (repository == "<none>" || repository == "") {
			repository = containerName
		}
		item := models.SoftwareObservedItem{
			Category: models.SoftwareCategoryContainer, Name: repository,
			Version: imageLabelsByVersion[imageID], ContainerName: containerName,
			ImagePullability: imagePullability(repository),
			ContainerTag:     tag,
			ContainerDigest:  digest,
			ContainerRunning: fields[4] == "running",
			BelongsTo:        "docker",
			ListeningPorts:   parseContainerPublishedPorts(fields[3]),
		}
		if digest != "" {
			item.PackageURL = dockerPackageURL(repository, digest, tag)
		}
		items = append(items, item)
	}
	return items, unusedImageCount
}

func appendPOSIXContainerItems(
	collection *ParsedCollection, sections map[string]*parsedSection, commandSetVersion string,
) {
	if commandSetVersion != POSIXCommandSetVersion {
		collection.Items = append(collection.Items, buildLinuxContainerItems(
			sectionOrEmpty(sections, "container-images"),
			sectionOrEmpty(sections, "containers"))...)
		return
	}
	items, unusedImageCount := buildPOSIXV3ContainerItems(
		sectionOrEmpty(sections, "container-images"),
		sectionOrEmpty(sections, "containers-v3"),
		sectionOrEmpty(sections, "container-image-refs"),
		sectionOrEmpty(sections, "container-image-labels"))
	collection.Items = append(collection.Items, items...)
	collection.UnusedImageCount = unusedImageCount
}

func normalizeContainerImageID(value string) string {
	return strings.TrimPrefix(value, "sha256:")
}

func cleanContainerValue(value string) string {
	if value == "<none>" {
		return ""
	}
	return value
}

// parseContainerPublishedPorts 只读取 Docker 端口列里带箭头的主机端口映
// 射；容器内部暴露但没有发布到主机的端口不进入软件清单。
func parseContainerPublishedPorts(value string) []models.SoftwareListeningPort {
	ports := []models.SoftwareListeningPort{}
	for _, mapping := range strings.Split(value, ",") {
		mapping = strings.TrimSpace(mapping)
		hostMapping, containerMapping, present := strings.Cut(mapping, "->")
		if !present {
			continue
		}
		_, protocol, protocolPresent := strings.Cut(strings.TrimSpace(containerMapping), "/")
		if !protocolPresent {
			continue
		}
		hostMapping = strings.TrimSpace(hostMapping)
		address, portText, err := net.SplitHostPort(hostMapping)
		if err != nil {
			continue
		}
		port, err := strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			continue
		}
		if strings.HasPrefix(hostMapping, "[") && !strings.HasPrefix(address, "[") {
			address = "[" + address + "]"
		}
		portEntry := models.SoftwareListeningPort{
			Address: address, Port: port, Protocol: strings.ToLower(protocol),
			Exposure: exposureForListenAddress(address),
		}
		duplicate := false
		for _, existing := range ports {
			if existing.Address == portEntry.Address && existing.Port == portEntry.Port &&
				existing.Protocol == portEntry.Protocol {
				duplicate = true
				break
			}
		}
		if !duplicate {
			ports = append(ports, portEntry)
		}
	}
	return ports
}

// imagePullability 按镜像名第一段判断镜像能否从公开仓库拉取。没有仓库前
// 缀的镜像名默认来自 Docker 官方仓库；localhost、回环地址、私有网段和本
// 地域名视为不可公开拉取；其他仓库地址无法穷举，标为未知。
func imagePullability(repository string) models.SoftwareImagePullability {
	firstSegment := repository
	if beforeSlash, _, present := strings.Cut(repository, "/"); present {
		firstSegment = beforeSlash
	}
	if repository == "" || firstSegment == "<none>" {
		return models.SoftwareImagePullabilityNotPublic
	}
	if !strings.Contains(firstSegment, ".") && !strings.Contains(firstSegment, ":") &&
		!strings.EqualFold(firstSegment, "localhost") {
		return models.SoftwareImagePullabilityPublic
	}
	switch strings.ToLower(firstSegment) {
	case "docker.io", "registry-1.docker.io", "index.docker.io", "ghcr.io",
		"quay.io", "gcr.io", "registry.k8s.io", "public.ecr.aws", "mcr.microsoft.com":
		return models.SoftwareImagePullabilityPublic
	}

	host := firstSegment
	if strings.HasPrefix(host, "[") {
		if beforeBracketClose, _, present := strings.Cut(host, "]"); present {
			host = strings.TrimPrefix(beforeBracketClose, "[")
		}
	} else if strings.Count(host, ":") <= 1 {
		beforePort, _, present := strings.Cut(host, ":")
		if present {
			host = beforePort
		}
	}
	if host == "" {
		return models.SoftwareImagePullabilityNotPublic
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" {
		return models.SoftwareImagePullabilityNotPublic
	}
	for _, suffix := range []string{".local", ".internal", ".lan", ".home.arpa"} {
		if strings.HasSuffix(host, suffix) {
			return models.SoftwareImagePullabilityNotPublic
		}
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return models.SoftwareImagePullabilityUnknown
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || sharedAddressSpaceIPv4(ip) {
		return models.SoftwareImagePullabilityNotPublic
	}
	return models.SoftwareImagePullabilityUnknown
}

// sharedAddressSpaceIPv4 判断 IPv4 地址是否在 100.64.0.0/10 共享地址空间
// 里。这段地址用于运营商级网络地址转换，不属于公开镜像仓库地址。
func sharedAddressSpaceIPv4(ip net.IP) bool {
	ipv4 := ip.To4()
	return ipv4 != nil && ipv4[0] == 100 && ipv4[1] >= 64 && ipv4[1] <= 127
}

func containerReferenceMatchesImage(
	imageReference, repository, tag, digest, imageID string,
) bool {
	if containerRefMatchesImage(imageReference, repository, tag, digest) {
		return true
	}
	return digest != "" && imageReference == digest ||
		imageID != "" && normalizeContainerImageID(imageReference) == imageID
}
