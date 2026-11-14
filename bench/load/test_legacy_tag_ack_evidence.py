"""Version-bound original tag declaration controls; no artifact/native run."""
import copy
import unittest
import projection_evidence as gate
from test_cohort_peak_evidence import cohort_fixture


def declared_legacy_pair():
    packet,catalog=cohort_fixture()
    packet['binding']['cell_index']=72
    catalog['cells'][0]['index']=72
    role='tag';report=packet[role];bound=packet['binding'][role]
    bound['binary_sha256']=gate.V06410_BINARY_SHA256
    bound['plan']['binary_sha256']=gate.V06410_BINARY_SHA256
    report['plan']['binary_sha256']=gate.V06410_BINARY_SHA256
    report['comparison_identity']['binary_sha256']=gate.V06410_BINARY_SHA256
    owner=bound['owned_cohort_outer']['owned_cohort_peak']
    for body in (owner,owner['before'],owner['after']):body['binary_sha256']=gate.V06410_BINARY_SHA256
    report['readiness']['legacy_tag_ack_contract']=dict(version=1,source_revision=gate.BASELINE_TAG_REVISION,
        source_tree='655071eb061f2647bd819ef30e0f42116f1dfcbf',artifact_sha256=gate.V06410_BINARY_SHA256,
        archive_sha256=gate.V06410_ARCHIVE_SHA256,artifact_authority_sha256=gate.V06410_ARTIFACT_AUTHORITY_SHA256,
        asset_id=669760344,cell_index=72,kind='add_crossfile_call',policy='legacy_default_unavailable')
    for row in report['records']:
        row['ack_mode_evidence']=gate.V06410_POLICY_EVIDENCE
        row.pop('observed_ack_mode',None)
    return packet,catalog


class LegacyTagACKDeclarationContract(unittest.TestCase):
    def test_known_original_default_tag_declaration_preserves_memory_scope(self):
        packet,catalog=declared_legacy_pair()
        self.assertEqual(gate.validate_cohort_pair(packet,catalog)['total_owned_process_cohort_footprint'],'PASS')

    def test_wrong_or_unknown_declarations_never_borrow_candidate_policy(self):
        for name in ('candidate','wrong_hash','wrong_archive','wrong_policy','local','wrong_cell','missing_metadata','invented_full','unknown_evidence','boolean_version'):
            with self.subTest(name=name):
                packet,catalog=declared_legacy_pair();meta=packet['tag']['readiness']['legacy_tag_ack_contract']
                if name=='candidate':packet['candidate']['readiness']['legacy_tag_ack_contract']=copy.deepcopy(meta)
                if name=='wrong_hash':meta['artifact_sha256']='0'*64
                if name=='wrong_archive':meta['archive_sha256']='0'*64
                if name=='wrong_policy':meta['policy']='full'
                if name=='local':packet['tag']['cell']['ack_mode']='local'
                if name=='wrong_cell':meta['cell_index']=73
                if name=='missing_metadata':packet['tag']['readiness'].pop('legacy_tag_ack_contract')
                if name=='invented_full':packet['tag']['records'][0]['observed_ack_mode']='full'
                if name=='unknown_evidence':packet['tag']['records'][0]['ack_mode_evidence']='inferred full'
                if name=='boolean_version':meta['version']=True
                with self.assertRaises((gate.Unbound,KeyError,TypeError,ValueError)):
                    gate.validate_cohort_pair(packet,catalog)

if __name__=='__main__':unittest.main()
