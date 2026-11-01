use bao_tree::{io::outboard::PreOrderMemOutboard, BlockSize};
fn main() {
    let sizes: [usize; 16] = [0, 1, 1023, 1024, 1025, 16383, 16384, 16385, 32768, 32769, 49157, 65536, 100000, 262144, 1048576, 1048577 + 16384 * 3];
    println!("# size root_hex preorder_outboard_hex (bao-tree 0.17.0, BlockSize::from_chunk_log(4), data[i] = i % 251)");
    for s in sizes {
        let data: Vec<u8> = (0..s).map(|i| (i % 251) as u8).collect();
        let ob = PreOrderMemOutboard::create(&data, BlockSize::from_chunk_log(4));
        assert_eq!(ob.root, blake3::hash(&data));
        let o = if ob.data.is_empty() { "-".to_string() } else { hex::encode(&ob.data) };
        println!("{} {} {}", s, ob.root.to_hex(), o);
    }
}
