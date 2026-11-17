"""只读封存 scene 补邮件密钥之前的正式运行回执；不修改原配置、不重建容器。"""
from pathlib import Path
import importlib.util
import json

ROOT = Path(__file__).resolve().parents[2]
OUT = ROOT / 'build/verification/gm-admin-20261117/mail'

def main():
    spec = importlib.util.spec_from_file_location('qualified_startup', ROOT / 'tools/scripts/start_docker_debug.py')
    startup = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(startup)
    receipt = startup.preflight()
    scenes = startup.run(startup.DOCKER + ['ps', '-q', '--filter', 'label=com.docker.compose.project=' + startup.PROJECT,
                                           '--filter', 'label=com.docker.compose.service=scene']).split()
    startup.require(len(scenes) == 1, 'Expected exactly one live scene container')
    scene = startup.inspect(scenes)[0]
    mail = startup.inspect(['xuanming-gm-mail-mail-1'])[0]
    scene_env = dict(x.split('=', 1) for x in scene['Config']['Env'])
    mail_env = dict(x.split('=', 1) for x in mail['Config']['Env'])
    original_env = startup.env_values(startup.ENV.read_bytes())
    private_env = startup.env_values((OUT / 'private/scene.env').read_bytes())
    key = 'MMORPG_ASSET_OP_SECRET_MAIL'
    startup.require(len(private_env.get(key, '')) >= 32 and private_env[key] == mail_env.get(key),
                    'Private scene key does not match the running mail producer')
    startup.require(key not in original_env or original_env[key] == private_env[key], 'Original environment has a conflicting mail key')
    startup.require(key not in scene_env or scene_env[key] == private_env[key], 'Current scene has a conflicting mail key')
    startup.require(scene['Id'] == receipt['contract']['ids']['scene'], 'Scene changed during preflight')
    startup.require(mail['State']['Health']['Status'] == 'healthy', 'Mail is not healthy')
    files = [*startup.COMPOSE_FILES,
             ROOT / 'build/verification/authored-festival-pet-acquisition-20261116/compose.authored.json',
             startup.recovery_path()]
    command = startup.DOCKER + ['compose', '--project-name', startup.PROJECT, '--env-file', str(startup.ENV)]
    for path in files:
        command += ['-f', str(path)]
    command += ['up', '-d', '--no-deps', '--no-build', '--pull', 'never', '--force-recreate', 'scene']
    plan = {
        'preparedAt': startup.now(), 'executed': False, 'sceneContainerCount': 1,
        'affectedServices': ['scene'], 'sceneContainerId': scene['Id'], 'sceneImageId': scene['Image'],
        'sceneStartedAt': scene['State']['StartedAt'], 'mailContainerId': mail['Id'],
        'privateSceneKeyMatchesRunningMail': True, 'originalEnvKeyPresent': key in original_env,
        'sceneKeyPresent': key in scene_env, 'secretVariable': key,
        'secretSourcePath': str(OUT / 'private/scene.env'),
        'sourceComposePath': str(startup.COMPOSE_FILES[0]),
        'originalPrivateEnvPath': str(startup.ENV),
        'originalPrivateEnvSha256': receipt['contract']['envSha256'],
        'sourceComposeSha256': receipt['contract']['composeSha256'][str(startup.COMPOSE_FILES[0])],
        'applyCommandAfterReviewedConfigChanges': command,
        'requiresExactRuntimeReceiptMatchImmediatelyBeforeMutation': True,
        'disconnectsCurrentScenePlayers': True,
        'restartAloneDoesNotProveAttachmentClaim': True,
        'standaloneFifthComposeOverlayRejectedByCurrentStartupGuard': True,
    }
    (OUT / 'scene-runtime-before.json').write_text(json.dumps(receipt, indent=2), encoding='utf-8')
    (OUT / 'scene-restart-plan.json').write_text(json.dumps(plan, indent=2), encoding='utf-8')
    print(json.dumps({'status': 'prepared', 'affectedSceneContainers': 1, 'applied': False,
                      'privateSceneKeyMatchesRunningMail': True, 'existingGuardPassed': True}))

if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        print(json.dumps({'status': 'failed', 'kind': type(error).__name__}))
        raise SystemExit(1)
