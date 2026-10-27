import type { PackBuild } from '@/lib/api/packs';
import { JAVA_IMAGES, recommendJavaForVersion } from '@/lib/javaVersion';

export function packServerGuide(build: Pick<PackBuild, 'minecraft' | 'loader' | 'loaderVersion'>, os: 'windows' | 'linux') {
    const safeVersion = /^[0-9][A-Za-z0-9._+-]*$/;
    const valid = safeVersion.test(build.minecraft) && (!build.loaderVersion || safeVersion.test(build.loaderVersion));
    const java = build.minecraft.startsWith('1.17') ? 'Java 16' : JAVA_IMAGES.find(image => image.id === recommendJavaForVersion(build.minecraft))?.label || 'the Java version required by this Minecraft release';
    const result = { java, install: '', start: '', memory: '', url: '', supported: valid };
    if (!valid) return { ...result, supported: false };
    switch (build.loader) {
        case 'fabric':
            return { ...result, url: 'https://fabricmc.net/use/installer/', install: `java -jar fabric-installer.jar server -mcversion ${build.minecraft}${build.loaderVersion ? ` -loader ${build.loaderVersion}` : ''} -downloadMinecraft`, start: 'java -Xms1G -Xmx4G -jar fabric-server-launch.jar nogui', memory: '4 GB is an example. Adjust the Java memory arguments to fit this pack and your host.' };
        case 'forge':
        case 'neoforge': {
            const modern = build.loader === 'neoforge' || Number(build.minecraft.split('.')[1]) >= 17 || Number(build.minecraft.split('.')[0]) >= 26;
            return { ...result, url: build.loader === 'forge' ? 'https://files.minecraftforge.net/' : 'https://neoforged.net/', install: `java -jar ${build.loader}-installer.jar --installServer`, start: modern ? (os === 'windows' ? '.\\run.bat nogui' : 'bash ./run.sh nogui') : '', memory: modern ? 'Set the memory limit in user_jvm_args.txt, for example -Xmx4G. Adjust it to your pack and host.' : 'Legacy Forge: launch the installed Forge server JAR, using its actual filename and the memory arguments recommended for this release.' };
        }
        case 'vanilla':
        case '':
            return { ...result, url: 'https://www.minecraft.net/en-us/download/server', start: 'java -Xms1G -Xmx4G -jar server.jar nogui', memory: '4 GB is an example. Download the exact Minecraft server release and name it server.jar.' };
        default:
            return { ...result, supported: false };
    }
}
