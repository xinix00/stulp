//go:build tamago

package appinstall

// Op een node zijn er geen bundels op schijf om op te ruimen: een app is een
// image dat HOP in een slot plaatst, en wat er van hem in het document staat is
// het manifest dat hij zelf meebracht. Een uninstall daar is dus klaar zodra het
// document bij is, en dat is precies wat "niets verwijderd, geen fout" zegt.
func RemoveBundle(_, _ string) (bool, error) { return false, nil }
