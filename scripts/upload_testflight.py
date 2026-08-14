#!/usr/bin/env python3
import os
import sys
import base64
import re
import subprocess
import tempfile
import zipfile
import shutil
import plistlib

def extract_entitlements_from_provision(prov_path):
    if not os.path.exists(prov_path):
        return None
    with open(prov_path, 'rb') as f:
        content = f.read()
    start = content.find(b'<?xml')
    end = content.find(b'</plist>')
    if start == -1 or end == -1:
        return None
    plist_data = content[start:end + len('</plist>')]
    pl = plistlib.loads(plist_data)
    return pl.get('Entitlements', {})

def sign_item(item_path, entitlements_dict, identity, keychain=None):
    ent_file = tempfile.mktemp(suffix='.plist')
    with open(ent_file, 'wb') as f:
        plistlib.dump(entitlements_dict, f)
    
    cmd = [
        'codesign', '--force', '--sign', identity,
        '--entitlements', ent_file,
        '--generate-entitlement-der',
        '--timestamp=none'
    ]
    if keychain:
        cmd += ['--keychain', keychain]
    cmd.append(item_path)
    print(f'Signing {os.path.basename(item_path)} with {len(entitlements_dict)} entitlements...')
    subprocess.run(cmd, check=True)
    if os.path.exists(ent_file):
        os.remove(ent_file)

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
            shutil.copyfile(repo_key, target_key)
            print(f'Copied key from repository {repo_key} into {target_key}')

    if not os.path.exists(target_key):
        print(f'::warning ::AuthKey_{key_id}.p8 not found. Skipping TestFlight upload.')
        sys.exit(0)

    if not ipa_path or not os.path.exists(ipa_path):
        print(f'::error ::IPA file not found at {ipa_path}')
        sys.exit(1)

    print('Sealing IPA with exact Provisioning Profile entitlements and DER encoding...')
    work_dir = tempfile.mkdtemp()
    with zipfile.ZipFile(ipa_path, 'r') as zip_ref:
        zip_ref.extractall(work_dir)

    app_dir = os.path.join(work_dir, 'Payload', 'OBhoD.app')
    ext_dir = os.path.join(app_dir, 'PlugIns', 'TunnelExtension.appex')
    identity = 'Apple Distribution: Sergei Bokarev (4Z5K8Y686M)'
    keychain = os.environ.get('KEYCHAIN_PATH')

    # Sign embedded frameworks if any
    fw_dir = os.path.join(app_dir, 'Frameworks')
    if os.path.isdir(fw_dir):
        for fw in os.listdir(fw_dir):
            fp = os.path.join(fw_dir, fw)
            c = ['codesign', '--force', '--sign', identity, '--timestamp=none']
            if keychain: c += ['--keychain', keychain]
            c.append(fp)
            subprocess.run(c, check=True)

    # 1. Sign Extension
    if os.path.exists(ext_dir):
        ext_prov = os.path.join(ext_dir, 'embedded.mobileprovision')
        if not os.path.exists(ext_prov):
            ext_prov = 'keys/OBhod_ext.mobileprovision'
        ext_ent = extract_entitlements_from_provision(ext_prov)
        if ext_ent:
            sign_item(ext_dir, ext_ent, identity, keychain)

    # 2. Sign Main App
    if os.path.exists(app_dir):
        app_prov = os.path.join(app_dir, 'embedded.mobileprovision')
        if not os.path.exists(app_prov):
            app_prov = 'keys/OBhod_app.mobileprovision'
        app_ent = extract_entitlements_from_provision(app_prov)
        if app_ent:
            sign_item(app_dir, app_ent, identity, keychain)

    print('=== VERIFYING CODESIGN OF APP ===')
    subprocess.run(['codesign', '-d', '--entitlements', ':-', app_dir])

    if os.path.exists(ext_dir):
        print('=== VERIFYING CODESIGN OF EXTENSION ===')
        subprocess.run(['codesign', '-d', '--entitlements', ':-', ext_dir])

    os.remove(ipa_path)
    with zipfile.ZipFile(ipa_path, 'w', zipfile.ZIP_DEFLATED) as zip_out:
        for root, _, files in os.walk(work_dir):
            for file in files:
                full_p = os.path.join(root, file)
                rel_p = os.path.relpath(full_p, work_dir)
                zip_out.write(full_p, rel_p)
    shutil.rmtree(work_dir)
    print(f'Re-packaged and sealed IPA ({os.path.getsize(ipa_path)} bytes). Starting altool upload...')

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
        sys.exit(res.returncode)
    else:
        print('🎉 SUCCESS! Application successfully uploaded to TestFlight!')

if __name__ == '__main__':
    main()
