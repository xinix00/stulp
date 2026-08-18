//go:build !tamago

package appinstall

// bundle.go — een app-bundel van schijf halen bij een uninstall.
//
// Dit is alles wat er over is van het installeren-vanaf-GitHub, en het is ook
// het enige deel dat nog ergens over ging: bestanden opruimen die Stulp zelf
// heeft neergezet. Installeren gebeurt niet meer hier — een app komt binnen
// doordat iemand hem neerzet (HOP plaatst een slot-image, docker start een
// container) en zich vervolgens meldt met zijn manifest. Zie
// internal/supervisor.offerApp.
//
// Alleen op een host: een node heeft geen bundels op schijf om op te ruimen.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RemoveBundle verwijdert de map van een app, maar alleen als die ónder appsRoot
// ligt en dus door Stulp is neergezet.
//
// Een root buiten die map is van de gebruiker -- `stulp install ./my-app` legde
// zijn werkboom vast als app-root -- en een uninstall hoort die te vergeten
// zonder hem aan te raken. Rapporteert of er echt iets weg is.
func RemoveBundle(appsRoot, root string) (bool, error) {
	if appsRoot == "" || root == "" {
		return false, nil
	}
	owned, resolved, err := owns(appsRoot, root)
	if err != nil || !owned {
		return false, err
	}
	if err := os.RemoveAll(resolved); err != nil {
		return false, fmt.Errorf("remove app bundle: %w", err)
	}
	return true, nil
}

// owns lost beide paden op dezelfde manier op vóór het vergelijken, zodat een
// gesymlinkte apps-map niet de uitkomst bepaalt.
func owns(appsRoot, root string) (bool, string, error) {
	installRoot, err := resolvePath(appsRoot)
	if err != nil {
		return false, "", err
	}
	resolved, err := resolvePath(root)
	if err != nil {
		return false, "", err
	}
	relative, err := filepath.Rel(installRoot, resolved)
	if err != nil {
		return false, "", nil
	}
	if relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false, "", nil
	}
	return true, resolved, nil
}

func resolvePath(value string) (string, error) {
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", value, err)
	}
	if evaluated, err := filepath.EvalSymlinks(absolute); err == nil {
		return evaluated, nil
	}
	return absolute, nil
}
