package main

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

// 在缓存池中查找相同名称的仓库，忽略.git后缀，返回仓库路径
func findSameName(name string) (string, error) {
	remoteList, err := os.ReadDir(StorePath)
	if err != nil {
		return "", fmt.Errorf("find same name: %w", err)
	}
	// 生成可能的仓库名变体
	possibleNames := []string{name}
	if strings.HasSuffix(name, ".git") {
		possibleNames = append(possibleNames, strings.TrimSuffix(name, ".git"))
	} else {
		possibleNames = append(possibleNames, name+".git")
	}
	// 遍历仓库列表
	for i := range remoteList {
		if !strings.Contains(remoteList[i].Name(), ".") {
			continue
		}
		ownerList, err := os.ReadDir(path.Join(StorePath, remoteList[i].Name()))
		if err != nil {
			return "", fmt.Errorf("find same name: %w", err)
		}
		// 遍历组织列表
		for j := range ownerList {
			for _, repoName := range possibleNames {
				repoPath := path.Join(StorePath, remoteList[i].Name(), ownerList[j].Name(), repoName)
				_, err := os.Stat(repoPath)
				if err == nil {
					return repoPath, nil
				}
			}
		}
	}
	return "", os.ErrNotExist
}

// 复制目录，符号链接，不复制文件
func copySymlink(src, dest string) error {
	files, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("read dir(%s): %w", src, err)
	}
	for i := range files {
		fname := files[i].Name()
		err = os.Link(filepath.Join(src, fname), filepath.Join(dest, fname))
		if err != nil {
			return fmt.Errorf("create symlink(%s): %w", dest, err)
		}
	}
	return nil
}

// 克隆仓库
func clone(remote, owner, repo string) error {
	url := fmt.Sprintf("https://%s/%s/%s", remote, owner, repo)
	localRepo := fmt.Sprintf("%s/%s/%s/%s", StorePath, remote, owner, repo)
	tempRepo := fmt.Sprintf("%s/%s/%s/%s.tmp", StorePath, remote, owner, repo)
	// 如果本地仓库存在，则使用本地仓库作为参考仓库
	referenceRepo := localRepo
	existsLocalRepo := false
	existsRefRepo := false
	_, err := os.Stat(localRepo)
	if err == nil {
		existsRefRepo = true
		existsLocalRepo = true
	} else {
		// 如果本地仓库不存在，则查找相同名称的仓库
		sameNameRepo, err := findSameName(repo)
		if err == nil {
			existsRefRepo = true
			referenceRepo = sameNameRepo
		}
	}
	// 先清理临时仓库
	err = os.RemoveAll(tempRepo)
	if err != nil {
		return fmt.Errorf("failed to remove temporary repository directory: %w", err)
	}
	// 克隆仓库，如果使用reference+dissociate选项会对从引用仓库复制pack并，影响clone速度
	// 所以采取先reference，然后硬链接pack文件，最后删除alternates断开引用，减少复制。
	// 缺点是随着clone次数增加，小pack会越来越多
	args := []string{"clone", "--bare", "--reference-if-able", referenceRepo, url, tempRepo}
	err = execCmd(exec.Command("git", args...))
	if err != nil {
		return fmt.Errorf("failed to clone repository: %w", err)
	}
	// 如果存在参考仓库，则复制符号链接
	if existsRefRepo {
		err = copySymlink(
			filepath.Join(referenceRepo, "objects/pack"),
			filepath.Join(tempRepo, "objects/pack"),
		)
		if err != nil {
			return err
		}
	}
	// 更新索引信息
	cmd := exec.Command("git", "update-server-info")
	cmd.Dir = tempRepo
	err = execCmd(cmd)
	if err != nil {
		return fmt.Errorf("failed to update server info: %w", err)
	}
	// 如果存在参考仓库，则删除参考仓库的符号链接
	if existsRefRepo {
		err = os.Remove(filepath.Join(tempRepo, "objects/info/alternates"))
		if err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("remove alternates: %w", err)
			}
		}
	}
	// 如果pack太多，进行一次重新打包
	files, err := os.ReadDir(filepath.Join(tempRepo, "/objects/pack"))
	if err == nil && len(files) > 500 {
		// 减少细碎文件
		cmd := exec.Command("git", "gc")
		cmd.Dir = tempRepo
		err = execCmd(cmd)
		if err != nil {
			return fmt.Errorf("failed to git gc: %w", err)
		}
	}
	// 先删除本地仓库，再重命名临时仓库为本地仓库
	if existsLocalRepo {
		err = os.RemoveAll(localRepo)
		if err != nil {
			return fmt.Errorf("failed to remove local repository directory: %w", err)
		}
	}
	err = os.Rename(tempRepo, localRepo)
	if err != nil {
		return fmt.Errorf("failed to rename temporary repository directory: %w", err)
	}
	return nil
}
