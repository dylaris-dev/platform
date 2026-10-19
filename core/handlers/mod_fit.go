package handlers

import "dylaris-core/services"

// The fit check lives in services so the dependency resolver there makes the
// same decision as the install guard here.

func modFitKnown(serverLoader, serverMC string) bool {
	return services.ModFitKnown(serverLoader, serverMC)
}

func modVersionFits(serverLoader, serverMC string, versionLoaders, versionGameVersions []string) (bool, string) {
	return services.ModVersionFits(serverLoader, serverMC, versionLoaders, versionGameVersions)
}
