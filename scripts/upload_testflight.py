#!/usr/bin/env python3
import os
import sys
import base64
import re
import subprocess

def main():
    key_id = os.environ.get('APP_STORE_CONNECT_API_KEY_ID') or '795KRDT33X'
    issuer_id = os.environ.get('APP_STORE_CONNECT_API_ISSUER_ID') or '7765344d-0858-4979-97e1-8266e18192db'
    ipa_path = os.environ.get('IPA_PATH')

    print('=== TESTFLIGHT UPLOADER ===')
    print(f'Key ID:    {key_id}')
    print(f'Issuer ID: {issuer_id}')
    print(f'IPA Path:  {ipa_path}')

    keys_dir = os.path.expanduser('~/.appstoreconnect/private_keys')
    os.makedirs(keys_dir, exist_ok=True)
    target_key = os.path.join(keys_dir, f'AuthKey_{key_id}.p8')

    raw_b64 = os.environ.get('APP_STORE_CONNECT_API_KEY_BASE64', '')
    if raw_b64:
        clean = re.sub(r'[^A-Za-z0-9+/=]', '', raw_b64)
        if len(clean) % 4 != 0:
            clean += '=' * (4 - len(clean) % 4)
        try:
            with open(target_key, 'wb') as f:
                f.write(base64.b64decode(clean))
            print(f'Successfully decoded key from BASE64 into {target_key}')
        except Exception as e:
            print(f'Warning: Failed to decode base64 key: {e}')

    if not os.path.exists(target_key):
        repo_key = os.path.join('keys', f'AuthKey_{key_id}.p8')
        if os.path.exists(repo_key):
            import shutil
            shutil.copyfile(repo_key, target_key)
            print(f'Copied key from repository {repo_key} into {target_key}')

    if not os.path.exists(target_key):
        print(f'::warning ::AuthKey_{key_id}.p8 not found. Skipping TestFlight upload.')
        sys.exit(0)

    if not ipa_path or not os.path.exists(ipa_path):
        print(f'::error ::IPA file not found at {ipa_path}')
        sys.exit(1)

    print(f'AuthKey exists ({os.path.getsize(target_key)} bytes). Starting altool upload...')

    cmd = [
        'xcrun', 'altool', '--upload-app',
        '--type', 'ios',
        '--file', ipa_path,
        '--apiKey', key_id,
        '--apiIssuer', issuer_id,
        '--verbose'
    ]

    res = subprocess.run(cmd)
    if res.returncode != 0:
        print(f'::warning ::altool finished with exit code {res.returncode}')
    else:
        print('SUCCESS! Application successfully uploaded to TestFlight!')

if __name__ == '__main__':
    main()
